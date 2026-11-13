package fhir

// Fetch (design §5.1, §7.1; plan Task 23): Observation (laboratory and
// vital signs), MedicationRequest and MedicationStatement by
// _lastUpdated=gt<cursor>, 200 a page, sorted by _lastUpdated; for each
// page, the Patient, Encounter and Location records it names, read from
// the same server by _id, at most 100 ids a request, put before the page's
// records. A page is complete when it and its references are stored; only
// then does the cursor move. At most MaxPages search pages or MaxRecords
// records a run; the rest continue next run. A first sync (no cursor for
// this base address) reads back BackfillDays.
//
// Refused whole, as unreadable_data: a page that isn't a searchset, has
// more entries than asked for, holds a resource of another type or without
// an id or lastUpdated, is out of _lastUpdated order (the server isn't
// sorting, so the cursor couldn't be trusted), has a lastUpdated more than
// 5 minutes ahead of our clock (it would carry the cursor past records not
// yet written), or names a next link anywhere but under the base address.

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
)

// maxAhead is how far a lastUpdated may be ahead of our clock.
const maxAhead = 5 * time.Minute

// run is one fetch's state.
type run struct {
	c       *FHIR
	ctx     context.Context
	client  *http.Client
	cfg     connector.Config
	creds   connector.Creds
	sink    connector.RawSink
	base    string
	now     time.Time
	pages   int
	records int
	maxP    int
	maxR    int
	seen    map[string]bool // references already put this run
}

func unreadable() error { return connector.Fail("unreadable_data", 0) }

// Fetch reads from cur on. On failure it returns the cursor of the pages
// completed so far with the error.
func (c *FHIR) Fetch(ctx context.Context, cfg connector.Config, creds connector.Creds, raw connector.Cursor, sink connector.RawSink) (connector.Cursor, error) {
	client, err := c.client(cfg)
	if err != nil {
		return raw, err
	}
	base := strings.TrimSuffix(cfg.BaseURL, "/")
	if !onOrigin(base, cfg.Origin) {
		return raw, signInRefused()
	}
	cur := c.cursorFor(raw, cfg)
	now := c.now()
	maxP, maxR, days := c.limits()
	since := now.Add(-time.Duration(days) * 24 * time.Hour)
	reconcile := parseTime(cur.From)
	from := map[string]time.Time{}
	for _, t := range SearchTypes {
		f := cur.at(t)
		if f.IsZero() {
			f = since
		}
		if !reconcile.IsZero() && reconcile.Before(f) {
			f = reconcile
		}
		from[t] = f
	}
	// The type furthest behind goes first, so a busy type can't starve the
	// others of the run's pages.
	order := slices.Clone(SearchTypes)
	sort.SliceStable(order, func(i, j int) bool { return from[order[i]].Before(from[order[j]]) })
	r := &run{c: c, ctx: ctx, client: client, cfg: cfg, creds: creds, sink: sink, base: base, now: now, maxP: maxP, maxR: maxR,
		seen: map[string]bool{}}
	progress := map[string]time.Time{}
	drained := true
	var ferr error
	for _, t := range order {
		done, err := r.search(t, from[t], func(lu time.Time) { progress[t] = lu })
		if err != nil {
			ferr, drained = err, false
			break
		}
		if !done {
			drained = false
			break
		}
	}
	out := cursor{V: cursorVersion, Base: cfg.BaseURL, LU: map[string]string{}}
	for _, t := range SearchTypes {
		at := cur.at(t)
		if p, ok := progress[t]; ok {
			if lu := p.Add(-Overlap); lu.After(at) {
				at = lu
			}
		}
		if !at.IsZero() {
			out.LU[t] = at.UTC().Format(time.RFC3339Nano)
		}
	}
	out.Drained = drained && ferr == nil
	return out.encode(), ferr
}

// search reads one type from since on; done is false when the run's
// limits stopped it.
func (r *run) search(typ string, since time.Time, completed func(time.Time)) (bool, error) {
	q := url.Values{}
	if typ == "Observation" {
		q.Set("category", "laboratory,vital-signs")
	}
	q.Set("_lastUpdated", "gt"+since.UTC().Format(time.RFC3339Nano))
	q.Set("_count", strconv.Itoa(PageCount))
	q.Set("_sort", "_lastUpdated")
	next := r.base + "/" + typ + "?" + q.Encode()
	var last time.Time
	for next != "" {
		if r.pages >= r.maxP || r.records >= r.maxR {
			return false, nil
		}
		r.pages++
		var b bundle
		if err := r.get(next, &b); err != nil {
			return false, err
		}
		if b.ResourceType != "Bundle" || b.Type != "searchset" || len(b.Entry) > PageCount {
			return false, unreadable()
		}
		var recs []connector.RawRecord
		for _, e := range b.Entry {
			if e.Search != nil && e.Search.Mode != "" && e.Search.Mode != "match" {
				continue // an included or outcome entry: not what was searched
			}
			rec, ok := mapResource(e.Resource, typ, r.base)
			if !ok {
				return false, unreadable()
			}
			lu := rec.SourceUpdatedAt
			if lu.Before(last) || lu.After(r.now.Add(maxAhead)) {
				return false, unreadable()
			}
			last = lu
			recs = append(recs, rec)
		}
		n := b.next()
		if n != "" && !underBase(n, r.base) {
			// The page itself is good: keep it, then refuse to go on.
			if err := r.store(recs); err != nil {
				return false, err
			}
			if len(recs) > 0 {
				completed(last)
			}
			return false, unreadable()
		}
		if err := r.store(recs); err != nil {
			return false, err
		}
		if len(recs) > 0 {
			completed(last)
		}
		next = n
	}
	return true, nil
}

// store puts a page: its references first (locations, patients,
// encounters), then its records.
func (r *run) store(recs []connector.RawRecord) error {
	var patients, encounters []string
	for _, rec := range recs {
		if p, _ := rec.Fields["patient"].(string); p != "" {
			patients = append(patients, p)
		}
		if e, _ := rec.Fields["encounter"].(string); e != "" {
			encounters = append(encounters, e)
		}
	}
	encs, err := r.read("Encounter", encounters)
	if err != nil {
		return err
	}
	var locations []string
	for _, e := range encs {
		if p, _ := e.Fields["patient"].(string); p != "" {
			patients = append(patients, p)
		}
		if l, _ := e.Fields["location"].(string); l != "" {
			locations = append(locations, l)
		}
	}
	locs, err := r.read("Location", locations)
	if err != nil {
		return err
	}
	pats, err := r.read("Patient", patients)
	if err != nil {
		return err
	}
	for _, group := range [][]connector.RawRecord{locs, pats, encs, recs} {
		for _, rec := range group {
			if err := r.sink.Put(r.ctx, rec); err != nil {
				return err
			}
		}
	}
	r.records += len(recs)
	for _, group := range [][]connector.RawRecord{locs, pats, encs} {
		for _, rec := range group {
			r.seen[rec.Kind+"/"+rec.SourceID] = true
		}
	}
	return nil
}

// read fetches the typ records named by ids that this run hasn't put yet,
// MaxIDs a request. An entry not asked for is ignored.
func (r *run) read(typ string, ids []string) ([]connector.RawRecord, error) {
	kind := strings.ToLower(typ)
	var want []string
	for _, id := range ids {
		if idShape.MatchString(id) && !r.seen[kind+"/"+id] && !slices.Contains(want, id) {
			want = append(want, id)
		}
	}
	var out []connector.RawRecord
	for len(want) > 0 {
		n := min(len(want), MaxIDs)
		batch := want[:n]
		want = want[n:]
		q := url.Values{"_id": {strings.Join(batch, ",")}, "_count": {strconv.Itoa(MaxIDs)}}
		var b bundle
		if err := r.get(r.base+"/"+typ+"?"+q.Encode(), &b); err != nil {
			return nil, err
		}
		if b.ResourceType != "Bundle" || b.Type != "searchset" || len(b.Entry) > PageCount {
			return nil, unreadable()
		}
		got := map[string]bool{}
		for _, e := range b.Entry {
			rec, ok := mapResource(e.Resource, typ, r.base)
			if !ok || !slices.Contains(batch, rec.SourceID) || got[rec.SourceID] {
				continue
			}
			got[rec.SourceID] = true
			out = append(out, rec)
		}
	}
	return out, nil
}

// get reads one page with the connection's token; a refused token is
// dropped from the cache.
func (r *run) get(u string, v any) error {
	tok, err := r.c.token(r.ctx, r.client, r.cfg, r.creds)
	if err != nil {
		return err
	}
	err = r.c.getJSON(r.ctx, r.client, u, tok, v)
	if connector.CodeOf(err) == "sign_in_refused" {
		r.c.forget(r.cfg)
	}
	return err
}
