package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/text/unicode/norm"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
)

// core reference-library: the shared reference library on a host with no
// repository checkout (the live site; deploy/README.md, "Live site: the
// shared reference library"; docs/models/reference-library.md).
//
//	core reference-library load ARTEFACT   load a pinned artefact
//	core reference-library status          the loaded libraries
//
// It runs as the provisioning login (core-cli's DATABASE_URL) with SET
// LOCAL ROLE mhai_provision, whose 00056 grants reach library rows only
// (a library, no hospital, no patient fields). The artefact is the one
// services/intelligence/scripts/reflib_convert.py writes: a gzip'd JSON
// Lines file, a header then one row per line, each with our pinned
// embedder's vector, so nothing is embedded here.
//
// load fails closed, before anything is written, unless:
//   - the file's bytes (read once, from one descriptor, no symlink) have
//     the SHA-256, version and row count pinned for its library in
//     reflibPins, which mirrors models/reference-library.lock
//     (TestReflibPinsMatchLock);
//   - every row is a reference row: an id in the KB#nnnnnnn namespace,
//     metadata keys from a closed list (nothing patient-shaped), an approved
//     kind and source (openFDA drug labels, MedlinePlus health topics),
//     nothing naming the public demo's synthetic source ("Sample reference
//     (synthetic demo)") and no prompt delimiter or citation in its text.
//
// Then one READ COMMITTED transaction, as reflib_load.py's: the rows of
// every version of the library are deleted (their ids are reused), the
// previous version is retired, the new one is inserted and made active,
// and reference_library.loaded is appended to the audit chain. Loading the
// active version again changes nothing and writes no audit entry; a version
// already loaded with other bytes is refused; loading an earlier pinned
// version rolls back to it.
//
// It prints counts, the library's name, version and digest; never row text.

const reflibUsage = "usage: core reference-library load ARTEFACT | core reference-library status"

type reflibPin struct {
	Version, SHA256 string
	Count           int
}

// reflibPins is models/reference-library.lock, compiled in: the core image
// is built from services/ and can't read the repository's models/ folder.
// TestReflibPinsMatchLock keeps the two equal; a new version is pinned in
// both by one pull request.
var reflibPins = map[string]reflibPin{
	"medref": {Version: "1.0.0", SHA256: "d150e3807189bbe0412e1234426452320e22cef258f72d10e835bf6eab2206e7", Count: 23654},
}

const (
	reflibSchema       = "mhai.reflib/1"
	reflibMaxBytes     = 1 << 30
	reflibMaxLine      = 1 << 20
	reflibMaxContent   = 4100
	reflibMaxDim       = 4096
	reflibMaxRows      = 999_999
	reflibBatch        = 500
	reflibChunkType    = "guideline" // chunk_visible_to shows a shared guideline to everyone
	reflibDemoSource   = "Sample reference (synthetic demo)"
	reflibAuditAction  = "reference_library.loaded"
	reflibAuditEntity  = "reference_library"
	reflibAuditVia     = "cli"
	reflibRefusePrefix = "reference library refused: "
)

var (
	reflibNameRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
	reflibVersionRe = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)
	reflibChunkIDRe = regexp.MustCompile(`^KB#[1-9][0-9]{6}$`)

	// The approved kinds and their source names in an artefact.
	reflibSources = map[string]string{"drug_label": "openFDA", "health_topic": "MedlinePlus"}
	// The metadata keys a row may carry (reference_library.ROW_KEYS).
	reflibRowKeys = []string{"kind", "source", "title", "url", "licence", "set_id", "topic_id", "section",
		"safety_critical", "part", "audience", "retrieved", "attribution"}
	reflibHeaderKeys = []string{"schema", "name", "version", "licence", "attribution", "embedder", "dim", "count",
		"built_from", "retrieved"}
	// Text a row may never hold: the demo's synthetic source (folded to
	// lower case, spaces collapsed) and a prompt's delimiters or a citation
	// (folded to upper case, spaces removed), as reference_library.py.
	reflibDemoMarks    = []string{strings.ToLower(reflibDemoSource), "(synthetic demo)", "synthetic demo data"}
	reflibForgeryMarks = []string{"<<", ">>", "KB#"}
)

// reflibRefused is a refusal; its text is a reason code, never row content.
type reflibRefused string

func (r reflibRefused) Error() string { return reflibRefusePrefix + string(r) }

type reflibHeader struct {
	Name, Version, Licence, Attribution, Embedder string
	Dim, Count                                    int
}

type reflibRow struct {
	chunkID, content string
	metadata         []byte // the row's metadata JSON, checked
	embedding        []float32
}

func reflibCmd(ctx context.Context, args []string, url string, out io.Writer) error {
	switch {
	case len(args) == 2 && args[0] == "load" && !strings.HasPrefix(args[1], "-"):
		return reflibLoad(ctx, args[1], url, reflibPins, out)
	case len(args) == 1 && args[0] == "status":
		return reflibStatus(ctx, url, out)
	}
	return errors.New(reflibUsage)
}

// reflibRead returns the file's bytes, read once from one descriptor opened
// without following a link, and bounded.
func reflibRead(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, reflibRefused("unreadable")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, reflibRefused("not_a_file")
	}
	data, err := io.ReadAll(io.LimitReader(f, reflibMaxBytes+1))
	if err != nil {
		return nil, reflibRefused("unreadable")
	}
	if len(data) > reflibMaxBytes {
		return nil, reflibRefused("too_large")
	}
	return data, nil
}

// reflibLines yields the artefact's decompressed lines, each bounded.
func reflibLines(data []byte, fn func(n int, line []byte) error) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return reflibRefused("not_gzip")
	}
	defer gz.Close()
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 64*1024), reflibMaxLine)
	n := 0
	for sc.Scan() {
		if err := fn(n, sc.Bytes()); err != nil {
			return err
		}
		n++
	}
	if sc.Err() != nil {
		return reflibRefused("bad_row")
	}
	if n == 0 {
		return reflibRefused("bad_header")
	}
	return nil
}

// exactKeys decodes a JSON object whose keys are exactly want.
func exactKeys(line []byte, want []string) (map[string]json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(line, &obj) != nil || len(obj) != len(want) {
		return nil, false
	}
	for _, k := range want {
		if _, ok := obj[k]; !ok {
			return nil, false
		}
	}
	return obj, true
}

func reflibParseHeader(line []byte) (reflibHeader, error) {
	obj, ok := exactKeys(line, reflibHeaderKeys)
	if !ok {
		return reflibHeader{}, reflibRefused("bad_header")
	}
	var h reflibHeader
	var schema, builtFrom, retrieved string
	for k, dst := range map[string]any{"schema": &schema, "name": &h.Name, "version": &h.Version, "licence": &h.Licence,
		"attribution": &h.Attribution, "embedder": &h.Embedder, "dim": &h.Dim, "count": &h.Count,
		"built_from": &builtFrom, "retrieved": &retrieved} {
		if json.Unmarshal(obj[k], dst) != nil {
			return reflibHeader{}, reflibRefused("bad_header")
		}
	}
	ok = schema == reflibSchema && reflibNameRe.MatchString(h.Name) && reflibVersionRe.MatchString(h.Version) &&
		h.Licence != "" && len(h.Licence) <= 512 && h.Attribution != "" && len(h.Attribution) <= 2048 &&
		h.Embedder != "" && len(h.Embedder) <= 200 && h.Dim >= 1 && h.Dim <= reflibMaxDim &&
		h.Count >= 0 && h.Count <= reflibMaxRows
	if !ok {
		return reflibHeader{}, reflibRefused("bad_header")
	}
	return h, nil
}

// reflibStrings returns every string in a decoded JSON value, keys too.
func reflibStrings(v any, out []string) []string {
	switch x := v.(type) {
	case string:
		out = append(out, x)
	case map[string]any:
		for k, e := range x {
			out = reflibStrings(e, append(out, k))
		}
	case []any:
		for _, e := range x {
			out = reflibStrings(e, out)
		}
	}
	return out
}

// reflibDemoMarked: whether text names the demo's synthetic source.
func reflibDemoMarked(text string) bool {
	folded := strings.Join(strings.Fields(strings.ToLower(norm.NFKC.String(text))), " ")
	for _, m := range reflibDemoMarks {
		if strings.Contains(folded, m) {
			return true
		}
	}
	return false
}

// reflibForged: whether text holds a delimiter or citation sequence.
func reflibForged(text string) bool {
	folded := strings.Join(strings.Fields(strings.ToUpper(norm.NFKC.String(text))), "")
	for _, m := range reflibForgeryMarks {
		if strings.Contains(folded, m) {
			return true
		}
	}
	return false
}

func reflibParseRow(line []byte, dim int) (reflibRow, error) {
	obj, ok := exactKeys(line, []string{"chunk_id", "content", "metadata", "embedding"})
	if !ok {
		return reflibRow{}, reflibRefused("bad_row")
	}
	var r reflibRow
	var emb []float64
	if json.Unmarshal(obj["chunk_id"], &r.chunkID) != nil || json.Unmarshal(obj["content"], &r.content) != nil ||
		json.Unmarshal(obj["embedding"], &emb) != nil {
		return reflibRow{}, reflibRefused("bad_row")
	}
	n := utf8.RuneCountInString(r.content)
	if !reflibChunkIDRe.MatchString(r.chunkID) || n == 0 || n > reflibMaxContent || len(emb) != dim ||
		strings.ContainsRune(r.content, 0) {
		return reflibRow{}, reflibRefused("bad_row")
	}
	r.embedding = make([]float32, dim)
	for i, x := range emb {
		f := float32(x)
		if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
			return reflibRow{}, reflibRefused("bad_row")
		}
		r.embedding[i] = f
	}
	var meta map[string]any
	if json.Unmarshal(obj["metadata"], &meta) != nil || meta == nil {
		return reflibRow{}, reflibRefused("bad_row")
	}
	for k := range meta {
		if !slices.Contains(reflibRowKeys, k) {
			return reflibRow{}, reflibRefused("row_patient_or_unknown_field")
		}
	}
	kind, _ := meta["kind"].(string)
	source, _ := meta["source"].(string)
	if want, ok := reflibSources[kind]; !ok || source != want {
		return reflibRow{}, reflibRefused("row_kind_not_approved")
	}
	title, ok1 := meta["title"].(string)
	_, ok2 := meta["licence"].(string)
	if !ok1 || !ok2 || title == "" {
		return reflibRow{}, reflibRefused("bad_row")
	}
	for _, s := range append(reflibStrings(meta, nil), r.content) {
		if reflibDemoMarked(s) {
			return reflibRow{}, reflibRefused("row_demo_source")
		}
		if strings.ContainsRune(s, 0) {
			return reflibRow{}, reflibRefused("bad_row")
		}
	}
	if reflibForged(r.content) || reflibForged(title) {
		return reflibRow{}, reflibRefused("bad_row")
	}
	r.metadata = obj["metadata"]
	return r, nil
}

// reflibCheck verifies the bytes against the pins and every row, without
// keeping the rows: the header, and the digest.
func reflibCheck(data []byte, pins map[string]reflibPin) (reflibHeader, string, error) {
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	var h reflibHeader
	seen := map[string]bool{}
	rows := 0
	err := reflibLines(data, func(n int, line []byte) error {
		if n == 0 {
			var err error
			if h, err = reflibParseHeader(line); err != nil {
				return err
			}
			pin, ok := pins[h.Name]
			switch {
			case !ok || pin.Version != h.Version:
				return reflibRefused("not_pinned")
			case pin.SHA256 != sha:
				return reflibRefused("hash_mismatch")
			case pin.Count != h.Count:
				return reflibRefused("count_mismatch")
			}
			return nil
		}
		r, err := reflibParseRow(line, h.Dim)
		if err != nil {
			return err
		}
		if seen[r.chunkID] {
			return reflibRefused("duplicate_id")
		}
		seen[r.chunkID] = true
		rows++
		return nil
	})
	if err != nil {
		return reflibHeader{}, "", err
	}
	if rows != h.Count {
		return reflibHeader{}, "", reflibRefused("count_mismatch")
	}
	return h, sha, nil
}

func reflibLoad(ctx context.Context, path, url string, pins map[string]reflibPin, out io.Writer) error {
	data, err := reflibRead(path)
	if err != nil {
		return err
	}
	h, sha, err := reflibCheck(data, pins)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "artefact %s %s: %d rows, sha256 %s, checked against the pin\n", h.Name, h.Version, h.Count, sha)
	pool, err := openOwner(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	var lib int64
	var already bool
	var replaced *string
	err = reflibTx(ctx, pool, func(tx pgx.Tx) error {
		var foundID int64
		var foundSHA string
		var foundActive bool
		err := tx.QueryRow(ctx, `SELECT id, sha256, active FROM reference_libraries WHERE name = $1 AND version = $2`,
			h.Name, h.Version).Scan(&foundID, &foundSHA, &foundActive)
		switch {
		case err == nil && foundSHA != sha:
			return reflibRefused("version_reloaded_with_other_bytes")
		case err == nil && foundActive:
			lib, already = foundID, true
			return nil
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("reference library: lookup failed")
		}
		var prev string
		if err := tx.QueryRow(ctx, `SELECT version FROM reference_libraries WHERE name = $1 AND active`, h.Name).Scan(&prev); err == nil {
			replaced = &prev
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("reference library: lookup failed")
		}
		// Every version's rows go first: their ids are reused.
		if _, err := tx.Exec(ctx, `DELETE FROM kb_chunks WHERE library_id IN (SELECT id FROM reference_libraries WHERE name = $1)`,
			h.Name); err != nil {
			return fmt.Errorf("reference library: could not remove the previous rows")
		}
		if foundID != 0 {
			if _, err := tx.Exec(ctx, `DELETE FROM reference_libraries WHERE id = $1`, foundID); err != nil {
				return fmt.Errorf("reference library: could not replace the earlier load")
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE reference_libraries SET active = false WHERE name = $1 AND active`, h.Name); err != nil {
			return fmt.Errorf("reference library: could not retire the previous version")
		}
		if err := tx.QueryRow(ctx, `INSERT INTO reference_libraries (name, version, sha256, licence, attribution, embedder, chunk_count, active)
			VALUES ($1, $2, $3, $4, $5, $6, 0, false) RETURNING id`,
			h.Name, h.Version, sha, h.Licence, h.Attribution, h.Embedder).Scan(&lib); err != nil {
			return fmt.Errorf("reference library: could not record the library")
		}
		n, err := reflibInsertRows(ctx, tx, data, h.Dim, lib)
		if err != nil {
			return err
		}
		if n != h.Count {
			return reflibRefused("count_mismatch")
		}
		if _, err := tx.Exec(ctx, `UPDATE reference_libraries SET active = true, chunk_count = $1 WHERE id = $2`, n, lib); err != nil {
			return fmt.Errorf("reference library: could not activate the library")
		}
		return audit.Write(ctx, tx, audit.Event{Action: reflibAuditAction, Entity: reflibAuditEntity,
			EntityID: strconv.FormatInt(lib, 10), Payload: map[string]any{"via": reflibAuditVia, "name": h.Name,
				"version": h.Version, "sha256": sha, "rows": n, "replaced": replaced}})
	})
	if err != nil {
		return err
	}
	if already {
		fmt.Fprintf(out, "%s %s is already the active version (library %d); nothing changed\n", h.Name, h.Version, lib)
		return nil
	}
	from := "none"
	if replaced != nil {
		from = *replaced
	}
	fmt.Fprintf(out, "loaded %s %s: %d shared rows (library %d, replaced %s); audited %s\n", h.Name, h.Version, h.Count, lib, from,
		reflibAuditAction)
	return nil
}

// reflibInsertRows inserts the artefact's rows in batches (COPY FROM is
// refused under row-level security); it parses the checked bytes again,
// never the file.
func reflibInsertRows(ctx context.Context, tx pgx.Tx, data []byte, dim int, lib int64) (int, error) {
	const q = `INSERT INTO kb_chunks (chunk_id, hospital_id, content, chunk_type, metadata_json, library_id, embedding)
		VALUES ($1, NULL, $2, $3, $4::jsonb, $5, $6)`
	n := 0
	b := &pgx.Batch{}
	flush := func() error {
		if b.Len() == 0 {
			return nil
		}
		err := tx.SendBatch(ctx, b).Close()
		b = &pgx.Batch{}
		if err != nil {
			return fmt.Errorf("reference library: could not insert the rows")
		}
		return nil
	}
	err := reflibLines(data, func(i int, line []byte) error {
		if i == 0 {
			return nil
		}
		r, err := reflibParseRow(line, dim)
		if err != nil {
			return err
		}
		b.Queue(q, r.chunkID, r.content, reflibChunkType, string(r.metadata), lib, r.embedding)
		n++
		if b.Len() >= reflibBatch {
			return flush()
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, flush()
}

// reflibTx runs fn in a READ COMMITTED transaction (audit.Write needs it)
// as mhai_provision.
func reflibTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return errors.New("reference library: begin failed")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_provision`); err != nil {
		return errors.New("reference library: DATABASE_URL must be the provisioning login (core-cli)")
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.New("reference library: commit failed")
	}
	return nil
}

// reflibStatus prints every loaded library version (metadata only).
func reflibStatus(ctx context.Context, url string, out io.Writer) error {
	pool, err := openOwner(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	found := 0
	err = reflibTx(ctx, pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT name, version, active, chunk_count, sha256, embedder, loaded_at::date::text
			FROM reference_libraries ORDER BY name, active DESC, loaded_at DESC`)
		if err != nil {
			return errors.New("reference library: status query failed")
		}
		defer rows.Close()
		for rows.Next() {
			var name, version, sha, embedder, at string
			var active bool
			var count int
			if err := rows.Scan(&name, &version, &active, &count, &sha, &embedder, &at); err != nil {
				return errors.New("reference library: status query failed")
			}
			state := "retired"
			if active {
				state = "active"
			}
			fmt.Fprintf(out, "%s %s %s: %d rows, sha256 %s, embedder %s, loaded %s\n", name, version, state, count, sha, embedder, at)
			found++
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	if found == 0 {
		fmt.Fprintln(out, "no reference library loaded")
	}
	return nil
}
