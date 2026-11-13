package connector_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector/fhir"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector/stub"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrtypes"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

// Synthetic, obviously fake.
const canary = "synthetic-test-secret-not-real-0002"

func creds() connector.Creds {
	return connector.NewCreds(map[string]seal.Secret{"api_key": seal.NewSecret([]byte(canary))})
}

// A3: Creds never print, by any path.
func TestCredsRedacted(t *testing.T) {
	c := creds()
	if string(c.Get("api_key").Bytes()) != canary || len(c.Get("missing").Bytes()) != 0 || !c.Has("api_key") || c.Has("x") {
		t.Fatal("Get/Has")
	}
	type wrap struct {
		C connector.Creds
		P *connector.Creds
	}
	outs := []string{fmt.Sprint(c), fmt.Sprintf("%v %+v %#v %s %q %x", c, c, c, c, c, c), c.String(), c.GoString(),
		fmt.Sprintf("%+v", wrap{c, &c}), fmt.Sprintf("%#v", wrap{c, &c}), fmt.Sprint(&c)}
	j, err := json.Marshal(wrap{c, &c})
	if err != nil {
		t.Fatal(err)
	}
	outs = append(outs, string(j))
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "creds", c, "p", &c)
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "creds", c)
	outs = append(outs, buf.String())
	for _, o := range outs {
		if strings.Contains(o, canary) || strings.Contains(o, "api_key") {
			t.Errorf("creds printed: %s", o)
		}
		if !strings.Contains(o, "[redacted]") {
			t.Errorf("no redaction mark: %s", o)
		}
	}
	c.Zero()
	if c.Has("api_key") {
		t.Fatal("Zero kept a field")
	}
}

// The stub exists only in development; nothing is offered unless enabled,
// and only built types are offered.
func TestStubOnlyInDevelopment(t *testing.T) {
	prod := connector.NewRegistry(false, stub.New())
	if _, ok := prod.Get("stub"); ok {
		t.Fatal("stub registered outside development")
	}
	if got := prod.Offered([]string{"stub", "fhir_r4"}); len(got) != 0 {
		t.Fatalf("offered outside development: %v (fhir_r4 not registered here)", got)
	}
	dev := connector.NewRegistry(true, stub.New())
	if _, ok := dev.Get("stub"); !ok {
		t.Fatal("stub missing in development")
	}
	if got := dev.Offered(nil); len(got) != 0 {
		t.Fatalf("offered with none enabled: %v", got)
	}
	got := dev.Offered([]string{"stub", "fhir_r4", "hl7v2_feed"})
	if len(got) != 1 || got[0].ID != "stub" {
		t.Fatalf("offered %v, want the stub only (the only one registered)", got)
	}
	// FHIR R4 is built (PR 6): registered in production too.
	both := connector.NewRegistry(false, stub.New(), fhir.New(nil))
	if got := both.Offered([]string{"stub", "fhir_r4"}); len(got) != 1 || got[0].ID != "fhir_r4" {
		t.Fatalf("production offers %v, want fhir_r4 only", got)
	}
}

// Every registered connector declares exactly its row of the shared table
// (gokit/emrtypes), so core's fields and the emr service's agree.
func TestConnectorsDeclareTheSharedTable(t *testing.T) {
	r := connector.NewRegistry(true, stub.New(), fhir.New(nil))
	for _, id := range emrtypes.Built(true) {
		c, ok := r.Get(id)
		if !ok {
			t.Fatalf("%s built but not registered", id)
		}
		want, _ := emrtypes.Lookup(id)
		got := c.Type()
		if got.ID != want.ID || got.Label != want.Label || !slices.Equal(got.Fields, want.Fields) {
			t.Errorf("%s declares %+v, the table has %+v", id, got, want)
		}
	}
}

// The stub's test: ok with a usable key, sign_in_refused with none or a
// placeholder; it counts its calls (for the A28 tests).
func TestStubTest(t *testing.T) {
	s := stub.New()
	cfg := connector.Config{Type: "stub", BaseURL: "https://stub.example.org", Settings: map[string]string{"client_id": "c"}}
	if err := s.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if r := s.Test(context.Background(), cfg, creds()); !r.OK || r.Code != "ok" {
		t.Fatalf("%+v", r)
	}
	for _, v := range []string{"changeme", "CHANGE-ME", "placeholder"} {
		c := connector.NewCreds(map[string]seal.Secret{"api_key": seal.NewSecret([]byte(v))})
		if r := s.Test(context.Background(), cfg, c); r.OK || r.Code != "sign_in_refused" {
			t.Fatalf("placeholder %q: %+v", v, r)
		}
	}
	if r := s.Test(context.Background(), cfg, connector.NewCreds(nil)); r.OK || r.Code != "sign_in_refused" {
		t.Fatalf("no creds: %+v", r)
	}
	if s.Calls() != 5 {
		t.Fatalf("calls %d", s.Calls())
	}
	if err := s.Validate(connector.Config{Type: "stub", Settings: map[string]string{}}); err == nil {
		t.Fatal("missing client_id accepted")
	}
}

func TestResultIsClosed(t *testing.T) {
	if r := connector.Result("ok"); !r.OK || r.Code != "ok" {
		t.Fatal(r)
	}
	if r := connector.Result("timed_out"); r.OK || r.Code != "timed_out" {
		t.Fatal(r)
	}
	if r := connector.Result("<script>"); r.OK || r.Code != "unreadable_data" {
		t.Fatalf("unknown code: %+v", r)
	}
}
