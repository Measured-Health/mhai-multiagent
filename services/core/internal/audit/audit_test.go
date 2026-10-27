package audit

import (
	"strings"
	"testing"
	"time"
)

func TestHashIsDeterministicAndKeyOrderFree(t *testing.T) {
	ts := time.Date(2026, 10, 27, 9, 0, 0, 123456000, time.UTC)
	r1 := record{ts: ts, action: "a", payload: []byte(`{"b":1,"a":{"y":2,"x":[1,"s"]}}`)}
	r2 := record{ts: ts, action: "a", payload: []byte(`{"a": {"x": [1, "s"], "y": 2}, "b": 1}`)}
	h1, err := r1.hash(genesis)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := r2.hash(genesis)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 || len(h1) != 64 {
		t.Fatalf("%s != %s", h1, h2)
	}
	if h3, _ := r1.hash(strings.Repeat("1", 64)); h3 == h1 {
		t.Fatal("hash ignores prev_hash")
	}
	r1.entity = "x"
	if h4, _ := r1.hash(genesis); h4 == h1 {
		t.Fatal("hash ignores entity")
	}
}
