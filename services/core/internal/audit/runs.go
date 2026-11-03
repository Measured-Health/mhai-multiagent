package audit

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The chain check runs as the platform app reads them (contract §14.9
// "Integrity"; mhai-web's VerifyRun and IntegrityStatus), the auditor's
// outcome note, and the undisplayable report. Every function takes the
// caller's mhai_platform transaction (authz.Tx).

// RunsLimit is a runs page.
const RunsLimit = 20

// OutcomeNoteMax is the longest outcome note (mhai-web's OUTCOME_NOTE_MAX,
// 00014's CHECK).
const OutcomeNoteMax = 1000

// UndisplayableMax is the most ids one report carries.
const UndisplayableMax = 50

// ErrConflict is an outcome note refused by the run's state: not broken,
// already noted, or a stale version.
var ErrConflict = errors.New("audit: run conflict")

// errUnshowable is a person name mhai-web wouldn't read; the reply fails
// closed rather than send it.
var errUnshowable = errors.New("audit: a run names a person who can't be shown")

// RunOut is a run as mhai-web's VerifyRun. from_checkpoint is always null
// until signed checkpoints exist.
type RunOut struct {
	ID             string      `json:"id"`
	StartedAt      string      `json:"started_at"`
	FinishedAt     *string     `json:"finished_at"`
	By             *PersonRef  `json:"by"`
	State          string      `json:"state"`
	EntriesChecked *int64      `json:"entries_checked"`
	FromCheckpoint any         `json:"from_checkpoint"`
	FromFirst      bool        `json:"from_first"`
	ToSeq          *int64      `json:"to_seq"`
	ToAt           *string     `json:"to_at"`
	FirstBrokenSeq *int64      `json:"first_broken_seq"`
	FirstBrokenID  *string     `json:"first_broken_id"`
	FirstBrokenAt  *string     `json:"first_broken_at"`
	ErrorCode      *string     `json:"error_code"`
	Outcome        *RunOutcome `json:"outcome"`
	Version        int         `json:"version"`
}

// RunOutcome is a broken run's outcome note.
type RunOutcome struct {
	Note string    `json:"note"`
	By   PersonRef `json:"by"`
	At   string    `json:"at"`
}

// IntegrityStatus is GET /platform/audit/verify/status, less the security
// contact, which the handler adds from configuration.
type IntegrityStatus struct {
	Latest       *RunOut `json:"latest"`
	Running      *RunOut `json:"running"`
	OpenFailure  *RunOut `json:"open_failure"`
	OldestKeptAt string  `json:"oldest_kept_at"`
}

const runColumns = `
	SELECT r.id, r.started_at, r.finished_at, r."by", ub.display_name, r.state, r.entries_checked, r.from_first,
	       r.to_seq, r.to_at, r.first_broken_seq, r.first_broken_id, r.first_broken_at, r.error_code,
	       r.outcome_note, r.outcome_by, uo.display_name, r.outcome_at, r.version
	FROM audit_verify_runs r
	LEFT JOIN users ub ON ub.id = r."by"
	LEFT JOIN users uo ON uo.id = r.outcome_by`

func stamp(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}

func showablePerson(id uuid.UUID, name *string) (PersonRef, error) {
	if name == nil || !displayText(*name, nameMax) || holdsPseudonym(*name) {
		return PersonRef{}, errUnshowable
	}
	return PersonRef{ID: id.String(), Name: *name}, nil
}

func scanRuns(rows pgx.Rows) ([]RunOut, []time.Time, error) {
	defer rows.Close()
	var out []RunOut
	var starts []time.Time
	for rows.Next() {
		var (
			id                               uuid.UUID
			started                          time.Time
			finished, toAt, brokenAt, noteAt *time.Time
			by, noteBy                       *uuid.UUID
			byName, noteByName, note         *string
			state                            string
			entries, toSeq, brokenSeq        *int64
			fromFirst                        bool
			brokenID, code                   *string
			version                          int
		)
		if err := rows.Scan(&id, &started, &finished, &by, &byName, &state, &entries, &fromFirst, &toSeq, &toAt,
			&brokenSeq, &brokenID, &brokenAt, &code, &note, &noteBy, &noteByName, &noteAt, &version); err != nil {
			return nil, nil, fmt.Errorf("audit: runs: %w", err)
		}
		r := RunOut{
			ID: id.String(), StartedAt: *stamp(&started), FinishedAt: stamp(finished), State: state,
			EntriesChecked: entries, FromFirst: fromFirst, ToSeq: toSeq, ToAt: stamp(toAt),
			FirstBrokenSeq: brokenSeq, FirstBrokenID: brokenID, FirstBrokenAt: stamp(brokenAt), ErrorCode: code,
			Version: version,
		}
		if by != nil {
			p, err := showablePerson(*by, byName)
			if err != nil {
				return nil, nil, err
			}
			r.By = &p
		}
		if note != nil && noteBy != nil && noteAt != nil {
			p, err := showablePerson(*noteBy, noteByName)
			if err != nil {
				return nil, nil, err
			}
			r.Outcome = &RunOutcome{Note: *note, By: p, At: *stamp(noteAt)}
		}
		out = append(out, r)
		starts = append(starts, started)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("audit: runs: %w", err)
	}
	return out, starts, nil
}

// RunCursor is a runs page's position: the last run's start and id.
type RunCursor struct {
	StartedAt time.Time
	ID        uuid.UUID
}

// Encode is 48 lowercase hex characters (start in microseconds, then the
// id), so it never has the pseudonym's shape. It is not authenticated:
// a forged cursor only pages the runs (no patient data, nothing a first
// page and the run reads don't show, and those are recorded), and paging
// writes no entry (spec §5.2).
func (c RunCursor) Encode() string {
	var b [24]byte
	binary.BigEndian.PutUint64(b[:8], uint64(c.StartedAt.UnixMicro()))
	copy(b[8:], c.ID[:])
	return hex.EncodeToString(b[:])
}

// DecodeRunCursor parses Encode's form, or ErrCursor.
func DecodeRunCursor(s string) (RunCursor, error) {
	if len(s) != 48 || strings.ToLower(s) != s {
		return RunCursor{}, ErrCursor
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return RunCursor{}, ErrCursor
	}
	us := int64(binary.BigEndian.Uint64(b[:8]))
	if us <= 0 {
		return RunCursor{}, ErrCursor
	}
	var id uuid.UUID
	copy(id[:], b[8:])
	return RunCursor{StartedAt: time.UnixMicro(us).UTC(), ID: id}, nil
}

// ListRuns is a page of runs, newest first, after the cursor (nil: the
// first page). next is "" on the last page.
func ListRuns(ctx context.Context, tx pgx.Tx, after *RunCursor) (runs []RunOut, next string, err error) {
	q := runColumns
	args := []any{RunsLimit + 1}
	if after != nil {
		q += ` WHERE (r.started_at, r.id) < ($2, $3)`
		args = append(args, after.StartedAt, after.ID)
	}
	rows, err := tx.Query(ctx, q+` ORDER BY r.started_at DESC, r.id DESC LIMIT $1`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("audit: runs: %w", err)
	}
	runs, starts, err := scanRuns(rows)
	if err != nil {
		return nil, "", err
	}
	if len(runs) > RunsLimit {
		runs, starts = runs[:RunsLimit], starts[:RunsLimit]
		last := len(runs) - 1
		next = RunCursor{StartedAt: starts[last], ID: uuid.MustParse(runs[last].ID)}.Encode()
	}
	if runs == nil {
		runs = []RunOut{}
	}
	return runs, next, nil
}

func oneRun(ctx context.Context, tx pgx.Tx, where string, args ...any) (*RunOut, error) {
	rows, err := tx.Query(ctx, runColumns+` WHERE `+where+` ORDER BY r.started_at DESC, r.id DESC LIMIT 1`, args...)
	if err != nil {
		return nil, fmt.Errorf("audit: run: %w", err)
	}
	runs, _, err := scanRuns(rows)
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	return &runs[0], nil
}

// GetRun is one run, or ErrNotFound.
func GetRun(ctx context.Context, tx pgx.Tx, id uuid.UUID) (RunOut, error) {
	r, err := oneRun(ctx, tx, `r.id = $1`, id)
	if err != nil {
		return RunOut{}, err
	}
	if r == nil {
		return RunOut{}, ErrNotFound
	}
	return *r, nil
}

// Status is the integrity header's read: the latest finished run, the
// running one, the open failure (OpenFailure's run), and the oldest
// entry's time.
func Status(ctx context.Context, tx pgx.Tx) (IntegrityStatus, error) {
	var st IntegrityStatus
	var err error
	if st.Latest, err = oneRun(ctx, tx, `r.state <> 'running'`); err != nil {
		return st, err
	}
	if st.Running, err = oneRun(ctx, tx, `r.state = 'running'`); err != nil {
		return st, err
	}
	open, err := OpenFailure(ctx, tx)
	if err != nil {
		return st, err
	}
	if open != uuid.Nil {
		if st.OpenFailure, err = oneRun(ctx, tx, `r.id = $1`, open); err != nil {
			return st, err
		}
	}
	if st.OldestKeptAt, err = oldestKept(ctx, tx); err != nil {
		return st, err
	}
	return st, nil
}

// OutcomeNoteOK is the outcome note's check: valid UTF-8, not blank, at
// most 1000 characters and 1000 UTF-16 units, line breaks allowed but no
// other control character, no format character or line or paragraph
// separator (R11's rules), and nothing of the pseudonym's shape (C22).
func OutcomeNoteOK(s string) bool {
	if !utf8.ValidString(s) || strings.TrimSpace(s) == "" ||
		utf8.RuneCountInString(s) > OutcomeNoteMax || len(utf16.Encode([]rune(s))) > OutcomeNoteMax {
		return false
	}
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r':
		case unicode.Is(unicode.Cc, r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r),
			r == utf8.RuneError:
			return false
		}
	}
	return !holdsPseudonym(s)
}

// AddOutcome stores an auditor's outcome note on a broken run and writes
// audit.check_outcome {run_id} (never the note: C16), in tx. It is
// ErrNotFound for no such run, and ErrConflict when the run isn't broken,
// already has its note, or version isn't the run's. The note must pass
// OutcomeNoteOK and by is the verified user; the caller checks both. Call
// it last in tx (the chain lock is held until commit).
func AddOutcome(ctx context.Context, tx pgx.Tx, r Reader, runID uuid.UUID, note string, version int) (RunOut, error) {
	by, err := uuid.Parse(r.UserID)
	if err != nil {
		return RunOut{}, errors.New("audit: outcome without a user")
	}
	var state string
	var cur int
	var noted bool
	err = tx.QueryRow(ctx, `SELECT state, version, outcome_note IS NOT NULL FROM audit_verify_runs WHERE id = $1 FOR UPDATE`,
		runID).Scan(&state, &cur, &noted)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunOut{}, ErrNotFound
	}
	if err != nil {
		return RunOut{}, fmt.Errorf("audit: outcome: %w", err)
	}
	if state != "broken" || noted || cur != version {
		return RunOut{}, ErrConflict
	}
	if _, err := tx.Exec(ctx, `
		UPDATE audit_verify_runs SET outcome_note = $2, outcome_by = $3, outcome_at = now(), version = version + 1
		WHERE id = $1`, runID, note, by); err != nil {
		return RunOut{}, fmt.Errorf("audit: outcome: %w", err)
	}
	out, err := GetRun(ctx, tx, runID)
	if err != nil {
		return RunOut{}, err
	}
	if err := Write(ctx, tx, runEvent("audit.check_outcome", runID, &by, r.RequestID,
		map[string]any{"run_id": runID.String()})); err != nil {
		return RunOut{}, err
	}
	return out, nil
}

// RecordRunsRead records a read of the chain checks inside tx, the way
// RecordRead does the list's: audit.searched {searched_kind: run}, once
// per session and read per 5 minutes. what is "runs" (the list's first
// page; the handler records no page after it), "run" (one run; key its id, which goes in entity_id) or
// "status". mhai-web has no searched_kind run yet, so these rows are sent
// bare (TODO.md), as the person and hospital reads are.
func RecordRunsRead(ctx context.Context, tx pgx.Tx, r Reader, what, key string) (bool, error) {
	e := Event{Action: "audit.searched", Payload: map[string]any{"searched_kind": "run"}}
	switch what {
	case "runs", "status":
	case "run":
		e.Entity, e.EntityID = "audit_run", key
	default:
		return false, errors.New("audit: unknown runs read")
	}
	return recordRead(ctx, tx, r, "verify."+what, []byte(key), e)
}

// RecordUndisplayable writes one audit.undisplayable_reported for a report
// of 1 to 50 event ids: their count (distinct) and the first and last ids
// in seq order, never the list. ids are decimal event ids (EventID).
func RecordUndisplayable(ctx context.Context, tx pgx.Tx, r Reader, ids []int64) error {
	set := map[int64]bool{}
	for _, id := range ids {
		set[id] = true
	}
	if len(ids) == 0 || len(ids) > UndisplayableMax {
		return errors.New("audit: undisplayable report of the wrong size")
	}
	seqs := make([]int64, 0, len(set))
	for id := range set {
		seqs = append(seqs, id)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	if _, err := uuid.Parse(r.UserID); err != nil {
		return errors.New("audit: report without a user")
	}
	return Write(ctx, tx, Event{ActorID: r.UserID, RequestID: r.RequestID, Action: "audit.undisplayable_reported",
		Payload: map[string]any{"count": len(seqs), "first_id": strconv.FormatInt(seqs[0], 10),
			"last_id": strconv.FormatInt(seqs[len(seqs)-1], 10)}})
}
