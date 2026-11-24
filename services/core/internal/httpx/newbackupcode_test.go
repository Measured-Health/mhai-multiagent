package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewBackupCodeCarrier(t *testing.T) {
	const code = "synth-renew"
	cases := []struct {
		name    string
		act     func(s *NewBackupCodeSlot)
		status  int
		viaBody bool // Write without WriteHeader
		forge   bool // the handler sets the header itself
		want    bool
	}{
		{"nothing offered", func(*NewBackupCodeSlot) {}, 200, false, false, false},
		{"in-tx code, 2xx", func(s *NewBackupCodeSlot) { s.Offer(code, false) }, 200, false, false, true},
		{"in-tx code, 204", func(s *NewBackupCodeSlot) { s.Offer(code, false) }, 204, false, false, true},
		{"in-tx code, implicit 200", func(s *NewBackupCodeSlot) { s.Offer(code, false) }, 0, true, false, true},
		{"in-tx code, rolled back (500)", func(s *NewBackupCodeSlot) { s.Offer(code, false) }, 500, false, false, false},
		{"in-tx code, refused (403)", func(s *NewBackupCodeSlot) { s.Offer(code, false) }, 403, false, false, false},
		{"committed code, 500", func(s *NewBackupCodeSlot) { s.Offer(code, true) }, 500, false, false, true},
		{"committed code, 2xx", func(s *NewBackupCodeSlot) { s.Offer(code, true) }, 200, false, false, true},
		{"taken into the body", func(s *NewBackupCodeSlot) { s.Offer(code, true); _ = s.Take() }, 200, false, false, false},
		{"dropped (superseded)", func(s *NewBackupCodeSlot) { s.Offer(code, false); s.Drop() }, 200, false, false, false},
		{"forged by a handler", func(*NewBackupCodeSlot) {}, 200, false, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := carryNewBackupCode(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				s, ok := NewBackupCodeSlotFrom(r.Context())
				if !ok {
					t.Fatal("no slot in the request")
				}
				c.act(s)
				if c.forge {
					w.Header().Set(HeaderNewBackupCode, "forged")
				}
				if c.viaBody {
					_, _ = w.Write([]byte("{}"))
					return
				}
				w.WriteHeader(c.status)
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
			got := rec.Header().Get(HeaderNewBackupCode)
			if c.want {
				if got != code || rec.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("header %q, Cache-Control %q", got, rec.Header().Get("Cache-Control"))
				}
			} else if got != "" {
				t.Fatalf("header %q sent", got)
			}
		})
	}
	// The carrier's own no-store is told apart from a handler's (which the
	// idempotency store must keep honouring).
	for _, own := range []bool{false, true} {
		var slot *NewBackupCodeSlot
		h := carryNewBackupCode(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			slot, _ = NewBackupCodeSlotFrom(r.Context())
			if own {
				w.Header().Set("Cache-Control", "no-store")
			}
			slot.Offer(code, true)
			w.WriteHeader(200)
		}))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", nil))
		if slot.addedNoStore() == own {
			t.Fatalf("handler no-store %v: carrier added %v", own, slot.addedNoStore())
		}
	}
	if _, ok := NewBackupCodeSlotFrom(httptest.NewRequest(http.MethodGet, "/x", nil).Context()); ok {
		t.Fatal("a slot outside the carrier")
	}
}
