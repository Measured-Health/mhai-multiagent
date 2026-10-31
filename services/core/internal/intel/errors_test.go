package intel

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// TestEveryIntelligenceCodeMapped is the error table of spec §10.1 (the
// request/reply column): every worker code, plus a NATS timeout and no
// responders, maps to exactly this status, code and Retry-After. The table
// iterates authz.ErrorCodes(), the one list of worker codes, so a code
// added there without a mapping fails here.
func TestEveryIntelligenceCodeMapped(t *testing.T) {
	type want struct {
		status     int
		code       string
		retryAfter int
	}
	spec := map[string]want{
		"unauthorized":      {500, "internal_error", 0},
		"forbidden":         {500, "internal_error", 0},
		"invalid_request":   {500, "internal_error", 0},
		"not_found":         {404, "not_found", 0},
		"deadline_exceeded": {503, "unavailable", 0},
		"overloaded":        {503, "unavailable", 5},
		"unavailable":       {503, "unavailable", 0},
		// "—" in §10.1: a request/reply call is never cancelled by the
		// worker, so one that says so is unexpected.
		"cancelled": {500, "internal_error", 0},
		"internal":  {500, "internal_error", 0},
	}
	codes := authz.ErrorCodes()
	if len(codes) != 9 || len(spec) != len(codes) {
		t.Fatalf("authz.ErrorCodes has %d codes, the spec table %d", len(codes), len(spec))
	}
	check := func(name string, err error, w want) {
		t.Helper()
		code, ra := Code(err)
		if code != w.code || ra != w.retryAfter || httpx.Codes[code] != w.status {
			t.Errorf("%s: got %s (%d) Retry-After %d, want %s (%d) Retry-After %d",
				name, code, httpx.Codes[code], ra, w.code, w.status, w.retryAfter)
		}
	}
	for _, c := range codes {
		w, ok := spec[c.Code]
		if !ok {
			t.Errorf("worker code %q has no row in the spec table", c.Code)
			continue
		}
		if _, ok := workerCodes[c.Code]; !ok {
			t.Errorf("worker code %q has no mapping", c.Code)
		}
		for _, retryable := range []bool{false, true} {
			check(c.Code, &Error{Code: c.Code, Retryable: retryable}, w)
			// Wrapped, as a caller might.
			check("wrapped "+c.Code, fmt.Errorf("x: %w", &Error{Code: c.Code, Retryable: retryable}), w)
		}
	}
	if len(workerCodes) != len(codes) {
		t.Errorf("the mapping has %d codes, authz.ErrorCodes %d", len(workerCodes), len(codes))
	}
	check("NATS timeout", ErrTimeout, want{503, "unavailable", 0})
	check("NATS no responders", ErrNoResponders, want{503, "unavailable", 0})
	check("JOBS refused the publish", ErrBackpressure, want{503, "unavailable", 0})
	check("in-flight cap", ErrAtCap, want{429, "rate_limited", 0})
	check("core's own role check", ErrNotAllowed, want{403, "forbidden", 0})
	// An unknown code, or any other error, is 500 internal_error.
	check("unknown worker code", &Error{Code: "teapot"}, want{500, "internal_error", 0})
	check("empty worker code", &Error{}, want{500, "internal_error", 0})
	check("other error", errors.New("boom"), want{500, "internal_error", 0})
	if code, ra := Code(nil); code != "" || ra != 0 {
		t.Errorf("nil: %q %d", code, ra)
	}
}

// Every code Code can return is in the catalogue, so httpx.Error never
// falls back silently.
func TestCodesAreInCatalogue(t *testing.T) {
	for _, m := range workerCodes {
		if _, ok := httpx.Codes[m.code]; !ok {
			t.Errorf("%q is not in httpx.Codes", m.code)
		}
	}
	for _, err := range []error{ErrTimeout, ErrNoResponders, ErrBackpressure, ErrAtCap, ErrNotAllowed, ErrBadReply} {
		code, _ := Code(err)
		if httpx.Codes[code] == 0 || code == "" {
			t.Errorf("%v maps to %q", err, code)
		}
	}
	if httpx.Codes["unavailable"] != http.StatusServiceUnavailable {
		t.Fatal("unavailable is not 503")
	}
}

// The error text never carries anything but the code from the closed set.
func TestErrorTextIsTheCodeOnly(t *testing.T) {
	if got := (&Error{Code: "forbidden"}).Error(); got != "intel: worker answered forbidden" {
		t.Errorf("got %q", got)
	}
	if got := (&Error{Code: "some text a worker made up"}).Error(); got != "intel: worker answered an unknown code" {
		t.Errorf("got %q", got)
	}
}

// Refusal reports which worker codes are refusals (audited as
// intelligence.refused, spec §10.2).
func TestRefusal(t *testing.T) {
	for _, c := range authz.ErrorCodes() {
		_, ok := Refusal(&Error{Code: c.Code})
		want := c.Code == "unauthorized" || c.Code == "forbidden" || c.Code == "invalid_request"
		if ok != want {
			t.Errorf("%s: refusal %v, want %v", c.Code, ok, want)
		}
	}
	if _, ok := Refusal(ErrTimeout); ok {
		t.Error("a timeout is not a refusal")
	}
}

// The deadline of a row with no deadline_at is run_query's timeout, in Go
// (ReserveInFlight, SweepOnce) and in 00007's mhai_fail_expired_queries and
// its policies, which write it as interval '10 minutes'.
func TestRunQueryTimeoutMatchesMigration(t *testing.T) {
	op, ok := authz.OperationByName(runQuery)
	if !ok || op.Timeout != 10*time.Minute {
		t.Fatalf("run_query timeout %v: update 00007_jobs.sql's interval '10 minutes' with it", op.Timeout)
	}
	raw, err := os.ReadFile(filepath.Join("..", "db", "migrations", "00007_jobs.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), "created_at + interval '10 minutes'"); n != 3 {
		t.Fatalf("00007 names the default deadline %d times, want 3", n)
	}
}
