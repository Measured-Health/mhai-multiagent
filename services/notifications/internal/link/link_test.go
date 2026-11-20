package link

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/notifypb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/channel"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/deliver"
)

var t0 = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

type fakeStore struct {
	found      bool
	recErr     error
	reserveOK  bool
	reserveErr error
	mandatory  bool
	purpose    string
}

func (f *fakeStore) Recipient(_ context.Context, _, _ uuid.UUID, purpose string) (string, string, bool, error) {
	f.purpose = purpose
	return "synthetic-addr@example.test", "Synthetic General Hospital", f.found, f.recErr
}

func (f *fakeStore) Reserve(_ context.Context, _, _ uuid.UUID, mandatory bool, _ time.Time, _ deliver.Limits) (bool, time.Time, error) {
	f.mandatory = mandatory
	return f.reserveOK, time.Time{}, f.reserveErr
}

type fakeEmail struct {
	mu    sync.Mutex
	res   []channel.Outcome
	sends []channel.Message
}

func (e *fakeEmail) Name() string { return "email" }
func (e *fakeEmail) Send(_ context.Context, m channel.Message) (channel.Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sends = append(e.sends, m)
	o := channel.Accepted
	if len(e.res) > 0 {
		o, e.res = e.res[0], e.res[1:]
	}
	return channel.Result{Outcome: o}, nil
}

const secretLink = "https://app.example.test/reset-password?token=SYNTHlinkValue0123"

func rig() (*Handler, *fakeStore, *fakeEmail, *bytes.Buffer) {
	st, em, logs := &fakeStore{found: true, reserveOK: true}, &fakeEmail{}, &bytes.Buffer{}
	return &Handler{Store: st, Email: em, Log: slog.New(slog.NewJSONHandler(logs, nil)), Now: func() time.Time { return t0 },
		Sleep: func(context.Context, time.Duration) {}}, st, em, logs
}

var hospital = uuid.MustParse("22222222-2222-4222-8222-222222222222")

func claims() reqtoken.Claims { return reqtoken.Claims{Hid: hospital.String(), Sub: reqtoken.SvcCore} }

func req(user, purpose string) *notifypb.SendLinkRequest {
	return &notifypb.SendLinkRequest{UserId: user, Purpose: purpose, Link: secretLink, ExpiresAt: t0.Add(30 * time.Minute).Unix()}
}

// outcome is a reply's outcome, or the error's text.
func outcome(rep proto.Message, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	return rep.(*notifypb.SendLinkReply).GetOutcome()
}

// An invite and a reset go to the recipient the database names, with the
// link and the hospital's name, mandatory for the hourly caps; the log
// carries the kind, ids and outcome only (NT3, NT19).
func TestSendsOnce(t *testing.T) {
	h, st, em, logs := rig()
	user := uuid.NewString()
	for _, purpose := range []string{"invite", "password_reset"} {
		if got := outcome(h.Handle(context.Background(), claims(), req(user, purpose))); got != Accepted {
			t.Fatalf("%s: %s", purpose, got)
		}
		if st.purpose != purpose || !st.mandatory {
			t.Fatalf("%s: store asked %q, mandatory %v", purpose, st.purpose, st.mandatory)
		}
	}
	if len(em.sends) != 2 || em.sends[0].Link != secretLink || em.sends[0].Address != "synthetic-addr@example.test" ||
		em.sends[0].HospitalName != "Synthetic General Hospital" || em.sends[0].Kind != "invite" ||
		em.sends[0].Hospital != hospital || em.sends[0].Expires != "20 November 2026 10:30 UTC" {
		t.Fatalf("%+v", em.sends)
	}
	out := logs.String()
	for _, bad := range []string{"SYNTHlinkValue", "token=", "synthetic-addr", "Synthetic General"} {
		if strings.Contains(out, bad) {
			t.Errorf("the log carries %q", bad)
		}
	}
	if !strings.Contains(out, `"outcome":"accepted"`) || !strings.Contains(out, user) {
		t.Fatalf("log: %s", out)
	}
}

// NT8 and the refusals: nobody with a fresh token, a malformed request, no
// email channel, a database fault and a spent allowance send nothing.
func TestRefusals(t *testing.T) {
	user := uuid.NewString()
	for name, tc := range map[string]struct {
		edit func(*Handler, *fakeStore)
		req  *notifypb.SendLinkRequest
		want string
	}{
		"no fresh token":   {func(_ *Handler, s *fakeStore) { s.found = false }, req(user, "invite"), NoRecipient},
		"database down":    {func(_ *Handler, s *fakeStore) { s.recErr = errors.New("synthetic") }, req(user, "invite"), NotReady},
		"counter down":     {func(_ *Handler, s *fakeStore) { s.reserveErr = errors.New("synthetic") }, req(user, "invite"), NotReady},
		"recipient capped": {func(_ *Handler, s *fakeStore) { s.reserveOK = false }, req(user, "invite"), RateLimited},
		"email off":        {func(h *Handler, _ *fakeStore) { h.Email = nil }, req(user, "invite"), NotReady},
		"another purpose":  {nil, req(user, "mfa_reset"), Invalid},
		"not a uuid":       {nil, req("someone@example.test", "invite"), Invalid},
		"upper-case uuid":  {nil, req(strings.ToUpper(user), "invite"), Invalid},
		"no link":          {nil, &notifypb.SendLinkRequest{UserId: user, Purpose: "invite", ExpiresAt: t0.Add(time.Hour).Unix()}, Invalid},
		"expired":          {nil, &notifypb.SendLinkRequest{UserId: user, Purpose: "invite", Link: secretLink, ExpiresAt: t0.Unix()}, Invalid},
		"over seven days":  {nil, &notifypb.SendLinkRequest{UserId: user, Purpose: "invite", Link: secretLink, ExpiresAt: t0.Add(8 * 24 * time.Hour).Unix()}, Invalid},
		"over-long link":   {nil, &notifypb.SendLinkRequest{UserId: user, Purpose: "invite", Link: strings.Repeat("x", 2049), ExpiresAt: t0.Add(time.Hour).Unix()}, Invalid},
	} {
		h, st, em, _ := rig()
		if tc.edit != nil {
			tc.edit(h, st)
		}
		if got := outcome(h.Handle(context.Background(), claims(), tc.req)); got != tc.want || len(em.sends) != 0 {
			t.Errorf("%s: %s, %d sends", name, got, len(em.sends))
		}
	}
	h, _, _, _ := rig()
	if got := outcome(h.Handle(context.Background(), reqtoken.Claims{Hid: "not-a-hospital"}, req(user, "invite"))); got != Invalid {
		t.Fatal(got)
	}
	if got := outcome(h.Handle(context.Background(), claims(), &notifypb.HealthRequest{})); got != Invalid {
		t.Fatal(got)
	}
}

// Three tries at most: a retryable failure is tried again, a permanent
// one is not; 30 links a minute per hospital, the next minute afresh.
func TestRetriesAndHospitalLimit(t *testing.T) {
	h, _, em, _ := rig()
	em.res = []channel.Outcome{channel.Retryable, channel.Retryable, channel.Accepted}
	if got := outcome(h.Handle(context.Background(), claims(), req(uuid.NewString(), "invite"))); got != Accepted || len(em.sends) != 3 {
		t.Fatalf("%s after %d tries", got, len(em.sends))
	}
	h, _, em, _ = rig()
	em.res = []channel.Outcome{channel.Retryable, channel.Retryable, channel.Retryable, channel.Accepted}
	if got := outcome(h.Handle(context.Background(), claims(), req(uuid.NewString(), "invite"))); got != Failed || len(em.sends) != 3 {
		t.Fatalf("%s after %d tries", got, len(em.sends))
	}
	h, _, em, _ = rig()
	em.res = []channel.Outcome{channel.Permanent}
	if got := outcome(h.Handle(context.Background(), claims(), req(uuid.NewString(), "invite"))); got != Failed || len(em.sends) != 1 {
		t.Fatalf("permanent: %s after %d tries", got, len(em.sends))
	}
	h, _, _, _ = rig()
	for i := 0; i < PerHospitalPerMinute; i++ {
		if got := outcome(h.Handle(context.Background(), claims(), req(uuid.NewString(), "invite"))); got != Accepted {
			t.Fatalf("link %d: %s", i, got)
		}
	}
	if got := outcome(h.Handle(context.Background(), claims(), req(uuid.NewString(), "invite"))); got != RateLimited {
		t.Fatalf("31st: %s", got)
	}
	other := claims()
	other.Hid = uuid.NewString()
	if got := outcome(h.Handle(context.Background(), other, req(uuid.NewString(), "invite"))); got != Accepted {
		t.Fatalf("another hospital: %s", got)
	}
	h.Now = func() time.Time { return t0.Add(time.Minute) }
	if got := outcome(h.Handle(context.Background(), claims(), req(uuid.NewString(), "invite"))); got != Accepted {
		t.Fatalf("next minute: %s", got)
	}
	// Defaults: a real sleep that a cancelled context cuts short.
	d := &Handler{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.sleep(ctx, time.Hour)
	if d.now().IsZero() || d.log() == nil {
		t.Fatal("defaults")
	}
}
