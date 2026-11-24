package httpx

import (
	"context"
	"net/http"
	"sync"
)

// HeaderNewBackupCode carries the account's replacement backup code after
// a step-up spent the old one (auth.Service.StepUp). Never logged, never
// stored for an idempotent replay (the reply is marked no-store).
const HeaderNewBackupCode = "X-MHAI-New-Backup-Code"

// NewBackupCodeSlot holds, for one request, the replacement backup code a
// step-up made, until the reply's status is known. Installed for every
// request by the router (Handler and NATSHandler).
type NewBackupCodeSlot struct {
	mu      sync.Mutex
	code    string
	durable bool
	// noStore: the carrier added Cache-Control: no-store for the code
	// (the handler hadn't set it), so the idempotency store may still keep
	// the reply.
	noStore bool
}

func (s *NewBackupCodeSlot) addedNoStore() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.noStore
}

// WithNewBackupCodeSlot returns ctx with an empty slot, and the slot.
func WithNewBackupCodeSlot(ctx context.Context) (context.Context, *NewBackupCodeSlot) {
	s := &NewBackupCodeSlot{}
	return context.WithValue(ctx, newBackupCodeKey, s), s
}

// NewBackupCodeSlotFrom returns the request's slot; false outside the
// router, where a replacement code would have no way to reach the person.
func NewBackupCodeSlotFrom(ctx context.Context) (*NewBackupCodeSlot, bool) {
	s, ok := ctx.Value(newBackupCodeKey).(*NewBackupCodeSlot)
	return s, ok && s != nil
}

// Offer records code. durable is true when the transaction that stored it
// (and burned the old code) has already committed: the reply then carries
// it whatever its status. Otherwise it is stored in a transaction still
// open, and only a 2xx reply (sent after that commit) carries it. A later
// offer replaces an earlier one: the later rotation replaced the codes.
func (s *NewBackupCodeSlot) Offer(code string, durable bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.code, s.durable = code, durable
}

// Take empties the slot and returns what it held, for a handler that puts
// the code in its body instead of the header.
func (s *NewBackupCodeSlot) Take() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.code
	s.code, s.durable = "", false
	return c
}

// Drop empties the slot: the code was superseded (a regenerate replaced
// every code in the same transaction).
func (s *NewBackupCodeSlot) Drop() { _ = s.Take() }

// release returns the code to send with a reply of status, emptying the
// slot; "" when there is none or the reply must not carry it.
func (s *NewBackupCodeSlot) release(status int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, d := s.code, s.durable
	s.code, s.durable = "", false
	if c == "" || (!d && (status < 200 || status > 299)) {
		return ""
	}
	return c
}

// carryNewBackupCode gives the request a slot and, when the reply's header
// is written, adds HeaderNewBackupCode (with Cache-Control: no-store) if
// the slot holds a code the reply may carry. It also removes any such
// header a handler set itself.
func carryNewBackupCode(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, slot := WithNewBackupCodeSlot(r.Context())
		next.ServeHTTP(&backupCodeWriter{ResponseWriter: w, slot: slot}, r.WithContext(ctx))
	})
}

type backupCodeWriter struct {
	http.ResponseWriter
	slot  *NewBackupCodeSlot
	wrote bool
}

func (b *backupCodeWriter) WriteHeader(status int) {
	if !b.wrote {
		b.wrote = true
		h := b.ResponseWriter.Header()
		h.Del(HeaderNewBackupCode)
		if c := b.slot.release(status); c != "" {
			if !hasNoStore(h.Values("Cache-Control")) {
				b.slot.mu.Lock()
				b.slot.noStore = true
				b.slot.mu.Unlock()
			}
			h.Set(HeaderNewBackupCode, c)
			h.Set("Cache-Control", "no-store")
		}
	}
	b.ResponseWriter.WriteHeader(status)
}

func (b *backupCodeWriter) Write(p []byte) (int, error) {
	if !b.wrote {
		b.WriteHeader(http.StatusOK)
	}
	return b.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (b *backupCodeWriter) Unwrap() http.ResponseWriter { return b.ResponseWriter }
