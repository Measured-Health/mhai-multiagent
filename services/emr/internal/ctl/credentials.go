package ctl

// The credential ops (EMR design §6.2, §6.3; plan Task 10):
//
//   - put_credentials (platform admin, step-up): opens every sealed box
//     against the connection's current origin, read under the token's
//     hospital; any refusal stores nothing (all fields or none). Then the
//     store, then the metadata rows, in that order, with the store's new
//     versions dropped again if the metadata can't be written. The reply is
//     each field and its set time, never a value.
//   - wipe_credentials (platform admin): every secret of the connection
//     from the store, then its metadata rows; idempotent; wiped only after
//     both.
//
// The hospital is the verified token's (hid): a connection of another
// hospital isn't found, and the op answers nothing.

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/secrets"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/dbx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrtypes"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

// TestTimeout is a connection test's deadline (design §4).
const TestTimeout = 20 * time.Second

// maxFields bounds a put (no type declares more secrets).
const maxFields = 8

// Errors the handlers return: fixed values. The responder logs only
// handler_error and sends nothing.
var (
	errRequest    = errors.New("ctl: request does not fit")
	errConnection = errors.New("ctl: no such connection for this hospital")
	errSealed     = errors.New("ctl: a sealed field was refused")
	errStore      = errors.New("ctl: the secret store failed")
	errDatabase   = errors.New("ctl: the database failed")
)

// Credentials serves put_credentials, wipe_credentials and test.
type Credentials struct {
	Pool  *pgxpool.Pool
	Store secrets.Store
	// Keys are the sealing key pairs (one, or two during a rotation).
	Keys seal.Keys
	// Env is APP_ENV: the store's path segment, and development allows IP
	// literal addresses (test servers).
	Env        string
	Connectors *connector.Registry
	Now        func() time.Time
}

func (s *Credentials) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Credentials) development() bool { return s.Env == "development" }

// Handlers are the three ops by name.
func (s *Credentials) Handlers() map[string]Handler {
	return map[string]Handler{"put_credentials": s.Put, "wipe_credentials": s.Wipe, "test": s.Test}
}

// ids reads the token's hospital and the request's connection, both
// canonical and non-nil.
func ids(c reqtoken.Claims, connection string) (hid, conn uuid.UUID, err error) {
	hid, err1 := uuid.Parse(c.Hid)
	conn, err2 := uuid.Parse(connection)
	if err1 != nil || err2 != nil || hid == uuid.Nil || conn == uuid.Nil || hid.String() != c.Hid || conn.String() != connection {
		return uuid.Nil, uuid.Nil, errRequest
	}
	return hid, conn, nil
}

// connRow is a connection as the emr role reads it.
type connRow struct {
	cfg    connector.Config
	stored []emrurl.Origin
}

// loadConn reads the connection under its hospital (RLS): another
// hospital's id is not found.
func (s *Credentials) loadConn(ctx context.Context, hid, id uuid.UUID) (connRow, error) {
	var out connRow
	err := dbx.WithHospital(ctx, s.Pool, dbx.EMR, hid, func(tx pgx.Tx) error {
		var settings map[string]string
		var origin []string
		err := tx.QueryRow(ctx, `SELECT type, base_url, settings, credentials_origin, poll_seconds
			FROM emr_connections WHERE id = $1 AND hospital_id = $2`, id, hid).
			Scan(&out.cfg.Type, &out.cfg.BaseURL, &settings, &origin, &out.cfg.PollSeconds)
		if errors.Is(err, pgx.ErrNoRows) {
			return errConnection
		}
		if err != nil {
			return errDatabase
		}
		out.cfg.ConnectionID, out.cfg.HospitalID, out.cfg.Settings = id, hid, settings
		for _, o := range origin {
			out.stored = append(out.stored, emrurl.Origin(o))
		}
		slices.Sort(out.stored)
		return nil
	})
	return out, err
}

// Put is put_credentials.
func (s *Credentials) Put(ctx context.Context, c reqtoken.Claims, m proto.Message) (proto.Message, error) {
	req, ok := m.(*emrpb.PutCredentialsRequest)
	if !ok || s.Store == nil {
		return nil, errRequest
	}
	hid, id, err := ids(c, req.GetConnectionId())
	if err != nil {
		return nil, err
	}
	actor, err := uuid.Parse(c.Sub)
	if err != nil || actor == uuid.Nil {
		return nil, errRequest
	}
	fields := req.GetFields()
	if len(fields) == 0 || len(fields) > maxFields {
		return nil, errRequest
	}
	cn, err := s.loadConn(ctx, hid, id)
	if err != nil {
		return nil, err
	}
	ti, ok := emrtypes.Lookup(cn.cfg.Type)
	if !ok || len(cn.stored) == 0 {
		return nil, errConnection
	}
	now := s.now()
	// Open every box first: one refusal and nothing is stored.
	opened := make(map[string]seal.Secret, len(fields))
	defer func() {
		for _, v := range opened {
			v.Zero()
		}
	}()
	for _, f := range fields {
		if _, dup := opened[f.GetField()]; dup || !slices.Contains(ti.Secrets(), f.GetField()) ||
			len(f.GetBox()) <= seal.KeyIDLen || string(f.GetBox()[:seal.KeyIDLen]) != f.GetKid() {
			return nil, errSealed
		}
		v, err := seal.Open(s.Keys, f.GetBox(), seal.Binding{ConnectionID: id, Field: f.GetField(), Origin: cn.stored}, now)
		if err != nil {
			return nil, errSealed
		}
		opened[f.GetField()] = v
	}
	// The store, all or nothing.
	type written struct {
		p       secrets.Path
		version string
	}
	var done []written
	undo := func() {
		for _, w := range done {
			_ = s.Store.DeleteVersion(context.WithoutCancel(ctx), w.p, w.version)
		}
	}
	names := make([]string, 0, len(opened))
	for k := range opened {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		p := secrets.Path{Env: s.Env, Hospital: hid, Connection: id, Field: k}
		ver, err := s.Store.Put(ctx, p, opened[k])
		if err != nil {
			undo()
			return nil, errStore
		}
		done = append(done, written{p, ver})
	}
	origin := emrurl.Strings(cn.stored)
	err = dbx.WithHospital(ctx, s.Pool, dbx.EMR, hid, func(tx pgx.Tx) error {
		for _, w := range done {
			if _, err := tx.Exec(ctx, `INSERT INTO emr.credentials (connection_id, hospital_id, field, set_at, secret_version, set_by, origin)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (connection_id, field) DO UPDATE SET set_at = EXCLUDED.set_at,
					secret_version = EXCLUDED.secret_version, set_by = EXCLUDED.set_by, origin = EXCLUDED.origin`,
				id, hid, w.p.Field, now, w.version, actor, origin); err != nil {
				return errDatabase
			}
		}
		return nil
	})
	if err != nil {
		undo()
		return nil, errDatabase
	}
	out := &emrpb.PutCredentialsReply{}
	for _, k := range names {
		out.Fields = append(out.Fields, &emrpb.FieldSetAt{Field: k, SetAt: now.Format(time.RFC3339)})
	}
	return out, nil
}

// Wipe is wipe_credentials.
func (s *Credentials) Wipe(ctx context.Context, c reqtoken.Claims, m proto.Message) (proto.Message, error) {
	req, ok := m.(*emrpb.WipeCredentialsRequest)
	if !ok || s.Store == nil {
		return nil, errRequest
	}
	hid, id, err := ids(c, req.GetConnectionId())
	if err != nil {
		return nil, err
	}
	if _, err := s.loadConn(ctx, hid, id); err != nil {
		return nil, err
	}
	if err := s.Store.DeleteAll(ctx, hid, id); err != nil {
		return nil, errStore
	}
	if err := dbx.WithHospital(ctx, s.Pool, dbx.EMR, hid, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM emr.credentials WHERE connection_id = $1 AND hospital_id = $2`, id, hid)
		return err
	}); err != nil {
		return nil, errDatabase
	}
	return &emrpb.WipeCredentialsReply{Wiped: true}, nil
}
