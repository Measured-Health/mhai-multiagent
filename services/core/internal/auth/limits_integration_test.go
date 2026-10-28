//go:build integration

package auth_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/password"
)

// D1: at most 20 failures per login per hour across every client IP. The
// 21st attempt, from a fresh IP and with the right password, is locked, with
// the same reply as an unknown login. The owner's own success does not
// reset the ceiling.
func TestLoginCeilingAcrossIPs(t *testing.T) {
	e := newEnv(t)
	u := clinician(t)
	// A success first: it must not use up the ceiling.
	if r := e.login(t, u.email, testPassword, newIP()); r.code != 200 {
		t.Fatalf("first login %d %s", r.code, r.raw)
	}
	for i := 0; i < auth.MaxLoginFailuresPerLogin; i++ {
		if r := e.login(t, u.email, "wrong-password-"+uniq(), newIP()); r.code != 401 {
			t.Fatalf("failure %d: %d %s", i+1, r.code, r.raw)
		}
	}
	r := e.login(t, u.email, testPassword, newIP())
	if r.code != 429 || r.body["code"] != "login_locked" {
		t.Fatalf("over the ceiling: %d %s", r.code, r.raw)
	}
	// An unknown login over its ceiling gets the same reply.
	unknown := "synthetic.nobody." + uniq() + "@havana.example"
	for i := 0; i < auth.MaxLoginFailuresPerLogin; i++ {
		if r := e.login(t, unknown, "wrong-password-"+uniq(), newIP()); r.code != 401 {
			t.Fatalf("unknown failure %d: %d", i+1, r.code)
		}
	}
	g := e.login(t, unknown, "wrong-password", newIP())
	if g.code != 429 || g.body["code"] != "login_locked" {
		t.Fatalf("unknown over the ceiling: %d %s", g.code, g.raw)
	}
	sameReply(t, r, g)
}

// Successful sign-ins are not failures: many in an hour never lock.
func TestLoginCeilingCountsFailuresOnly(t *testing.T) {
	e := newEnv(t)
	u := clinician(t)
	for i := 0; i < auth.MaxLoginFailuresPerLogin+3; i++ {
		if r := e.login(t, u.email, testPassword, newIP()); r.code != 200 {
			t.Fatalf("login %d: %d %s", i+1, r.code, r.raw)
		}
	}
}

// A concurrent burst from many IPs gets at most 20 password checks.
func TestLoginCeilingBurst(t *testing.T) {
	e := newEnv(t)
	// A roomy gate: this test is about the ceiling, not the CPU count.
	e.svc.Argon = password.NewGate(64, time.Minute)
	u := clinician(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	counts := map[int]int{}
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := e.login(t, u.email, "wrong-password-"+uniq(), newIP())
			mu.Lock()
			counts[r.code]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	// Exactly the ceiling's worth of password checks, the rest locked, and
	// nothing refused for contention.
	if counts[401] != auth.MaxLoginFailuresPerLogin || counts[429] != 30-auth.MaxLoginFailuresPerLogin {
		t.Fatalf("burst: %v\n%s", counts, authErrors(e))
	}
}

// authErrors returns the service's error log lines (no secrets are logged).
func authErrors(e *env) string {
	var out []string
	for _, l := range strings.Split(e.logs.String(), "\n") {
		if strings.Contains(l, "ERROR") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// faultyKV fails every read, standing in for a KV fault or contention that
// outlasts the retries.
type faultyKV struct{ jetstream.KeyValue }

func (faultyKV) Get(context.Context, string) (jetstream.KeyValueEntry, error) {
	return nil, errors.New("synthetic kv fault")
}

// When the ceiling counter can't be written, sign-in fails closed (503)
// without checking the password, and gives the (login, IP) slot back: a
// counter fault never costs the owner a try, and never buys a guess.
func TestLoginCeilingFaultRefundsIPSlot(t *testing.T) {
	e := newEnv(t)
	u := clinician(t)
	ip := newIP()
	ceil := e.svc.Buckets.LoginCeiling
	e.svc.Buckets.LoginCeiling = faultyKV{ceil}
	for i := 0; i < auth.MaxLoginFailures+2; i++ {
		r := e.login(t, u.email, "wrong-password-"+uniq(), ip)
		if r.code != 503 || r.body["code"] != "not_ready" {
			t.Fatalf("ceiling fault %d: %d %s", i+1, r.code, r.raw)
		}
	}
	k, err := e.svc.LockoutKey(u.email, ip)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buckets.LoginAttempts.Get(context.Background(), k); !bus.IsNotFound(err) {
		t.Fatalf("(login, IP) counter after ceiling faults: %v", err)
	}
	e.svc.Buckets.LoginCeiling = ceil
	if r := e.login(t, u.email, testPassword, ip); r.code != 200 {
		t.Fatalf("after the fault: %d %s", r.code, r.raw)
	}
}

// Without the ceiling bucket, sign-in fails closed.
func TestLoginCeilingFailsClosed(t *testing.T) {
	e := newEnv(t)
	e.svc.Buckets.LoginCeiling = nil
	u := clinician(t)
	r := e.login(t, u.email, testPassword, newIP())
	if r.code != 503 || r.body["code"] != "not_ready" {
		t.Fatalf("no ceiling bucket: %d %s", r.code, r.raw)
	}
}

// M7: argon2 runs behind a process-wide gate. When every slot is busy past
// the wait, sign-in and password change answer 503 not_ready, and the
// refused attempt is not counted against the login.
func TestArgonGateBusy(t *testing.T) {
	e := newEnv(t)
	gate := password.NewGate(1, 30*time.Millisecond)
	e.svc.Argon = gate
	u := clinician(t)
	ip := newIP()
	release, err := gate.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < auth.MaxLoginFailures+1; i++ {
		r := e.login(t, u.email, testPassword, ip)
		if r.code != 503 || r.body["code"] != "not_ready" {
			t.Fatalf("busy gate: %d %s", r.code, r.raw)
		}
	}
	release()
	// Not counted: the same (login, IP) still signs in.
	r := e.login(t, u.email, testPassword, ip)
	if r.code != 200 {
		t.Fatalf("after release: %d %s", r.code, r.raw)
	}
	tok, _ := r.body["access_token"].(string)
	release, _ = gate.Acquire(context.Background())
	pr := e.do(t, req{method: "POST", path: "/auth/password", bearer: tok, ip: ip,
		body: map[string]any{"current_password": testPassword, "new_password": "a-much-longer-synthetic-passphrase-" + uniq()}})
	release()
	if pr.code != 503 || pr.body["code"] != "not_ready" {
		t.Fatalf("password change with busy gate: %d %s", pr.code, pr.raw)
	}
}
