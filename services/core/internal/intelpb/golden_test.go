package intelpb

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// The golden files are shared with the Python tests in
// services/intelligence/tests/boundary/test_golden_messages.py. Each side
// writes <Name>.<lang>.bin; both decode every .bin and compare it with
// <Name>.json. All values are synthetic.
const goldenDir = "../../../../testdata/proto-golden"

var update = flag.Bool("update", false, "write the Go golden files")

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func samples(t *testing.T) map[string]proto.Message {
	t.Helper()
	meta := &ResponseMeta{
		AuditFacts: []*AuditFact{{
			Action:   "query.run",
			Entity:   "clinical_query",
			EntityId: "00000000-0000-4000-8000-000000000001",
			Payload:  mustStruct(t, map[string]any{"agents": 5, "mode": "synthetic"}),
		}},
		ModelVersions: map[string]string{"nli": "synthetic-1", "embedder": "synthetic-2"},
		TimingsMs:     map[string]float64{"mask": 1.5, "generate": 250.25},
	}
	return map[string]proto.Message{
		"Error": &Error{Code: "deadline_exceeded", Retryable: true},
		"AuditFact": &AuditFact{
			Action:   "query.run",
			Entity:   "clinical_query",
			EntityId: "00000000-0000-4000-8000-000000000001",
			Payload:  mustStruct(t, map[string]any{"agents": 5, "mode": "synthetic"}),
		},
		"ResponseMeta": meta,
		"DeadLetter": &DeadLetter{
			JobId:           "00000000-0000-4000-8000-000000000002",
			Type:            "run_query",
			SubjectHospital: "00000000-0000-4000-8000-0000000000a1",
			Attempts:        3,
			Code:            "unauthorized",
			Reason:          "signature",
			Kid:             "synthetic-kid-1",
		},
		"RunQueryJob":   &RunQueryJob{QueryId: "00000000-0000-4000-8000-000000000003"},
		"HealthRequest": &HealthRequest{},
		"HealthResponse": &HealthResponse{
			Meta:          meta,
			ModelsReady:   true,
			DatabaseReady: false,
		},
		"PingRequest": &PingRequest{Text: "hello", HospitalId: "00000000-0000-4000-8000-0000000000a1"},
		"PingResponse": &PingResponse{
			Meta: meta,
			Text: "hello",
		},
		"GrantRequest": &GrantRequest{
			AccessToken: "synthetic-access-token",
			Operation:   "echo.ping",
			Subject:     "inference.v1.echo.ping",
			BodyHash:    "synthetic-body-hash",
			PatientIds:  []string{"00000000-0000-4000-8000-0000000000b1", "00000000-0000-4000-8000-0000000000b2"},
			ReplyPrefix: "_INBOX_bff.clinical.",
			RequestId:   "00000000-0000-4000-8000-000000000004",
			DeadlineMs:  5000,
		},
		"GrantResponse": &GrantResponse{RequestToken: "synthetic-request-token"},
		"GrantRefused":  &GrantRefused{Code: "forbidden"},
	}
}

func TestGoldenMessagesRoundTrip(t *testing.T) {
	marshal := proto.MarshalOptions{Deterministic: true}
	want := samples(t)

	if *update {
		for name, msg := range want {
			b, err := marshal.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(goldenDir, name+".go.bin"), b, 0o644); err != nil {
				t.Fatal(err)
			}
			jsonPath := filepath.Join(goldenDir, name+".json")
			if cur, err := os.ReadFile(jsonPath); err == nil {
				got := msg.ProtoReflect().New().Interface()
				if protojson.Unmarshal(cur, got) == nil && proto.Equal(got, msg) {
					continue // keep the file byte-for-byte; protojson output is not stable
				}
			}
			j, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(jsonPath, append(j, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	// The JSON is the shared source of truth: it must equal what Go builds.
	for name, msg := range want {
		raw, err := os.ReadFile(filepath.Join(goldenDir, name+".json"))
		if err != nil {
			t.Fatalf("%s.json: %v (run with -update)", name, err)
		}
		fromJSON := msg.ProtoReflect().New().Interface()
		if err := protojson.Unmarshal(raw, fromJSON); err != nil {
			t.Fatalf("%s.json: %v", name, err)
		}
		if !proto.Equal(fromJSON, msg) {
			t.Errorf("%s.json differs from the Go sample", name)
		}
	}

	// Every .bin, Go-written and Python-written, decodes to that value.
	bins, err := filepath.Glob(filepath.Join(goldenDir, "*.bin"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(bins)
	byName := map[string]map[string][]byte{}
	for _, p := range bins {
		base := filepath.Base(p)
		parts := strings.Split(base, ".")
		if len(parts) != 3 || (parts[1] != "go" && parts[1] != "py") {
			t.Errorf("unexpected golden file %s", base)
			continue
		}
		msg, ok := want[parts[0]]
		if !ok {
			t.Errorf("golden file %s has no Go sample", base)
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		got := msg.ProtoReflect().New().Interface()
		if err := proto.Unmarshal(b, got); err != nil {
			t.Errorf("%s: %v", base, err)
			continue
		}
		if !proto.Equal(got, msg) {
			t.Errorf("%s decodes to a different value", base)
		}
		if byName[parts[0]] == nil {
			byName[parts[0]] = map[string][]byte{}
		}
		byName[parts[0]][parts[1]] = b
	}

	// Both languages wrote every message, and with deterministic encoding the
	// bytes are identical.
	for name := range want {
		files := byName[name]
		if files["go"] == nil || files["py"] == nil {
			t.Errorf("%s: need both .go.bin and .py.bin (update both sides)", name)
			continue
		}
		if !bytes.Equal(files["go"], files["py"]) {
			t.Errorf("%s: Go and Python encode different bytes", name)
		}
	}
}
