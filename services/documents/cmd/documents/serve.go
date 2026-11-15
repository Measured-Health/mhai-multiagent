package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/config"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/ctl"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/docspb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/logx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/replay"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

// natsUser is the documents service's NATS user; inboxPrefix its own inbox
// (services/nats/documents.conf).
const (
	natsUser    = "documents"
	inboxPrefix = "_INBOX_worker.documents"
)

// pinger is what health needs of the database.
type pinger interface {
	Ping(ctx context.Context) error
}

// healthHandler answers documents.v1.ctl.health: readiness flags only, no
// counts of anyone's data. PR 1 has no keyring, scanner or sandbox, so
// those are reported as not ready and the service never claims it can
// take a document (design §17 departure 4).
func healthHandler(db pinger) ctl.Handler {
	return func(ctx context.Context, _ reqtoken.Claims, _ proto.Message) (proto.Message, error) {
		ready := false
		if db != nil {
			c, cancel := context.WithTimeout(ctx, 2*time.Second)
			ready = db.Ping(c) == nil
			cancel()
		}
		return &docspb.HealthReply{DatabaseReady: ready}, nil
	}
}

// newLogger is the service's logger: JSON, with gokit's redaction of
// credential and personal-data keys.
func newLogger(w io.Writer) *slog.Logger { return logx.New(w) }

// logStart logs the start with settings' shapes only, never their values.
func logStart(log *slog.Logger, env string, trustedKeys int) {
	log.Info("documents serving", "env", env, "subjects", ctl.Subjects, "trusted_keys", trustedKeys,
		"keyring_ready", false, "scanner_ready", false, "sandbox_ready", false)
}

func serve(getenv func(string) string) error {
	s, err := config.FromEnv(getenv)
	if err != nil {
		return err
	}
	log := newLogger(os.Stdout)
	keys, err := reqtoken.ReadTrusted(s.TrustedKeysPath)
	if err != nil {
		return errors.New("DOCUMENTS_TRUSTED_KEYS_PATH: the trusted request-keys file is missing or not valid")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The pool connects lazily: the service starts (and health answers
	// database_ready false) while the database or its login isn't there
	// (the login comes with PR 3's migration).
	cfg, err := pgxpool.ParseConfig(s.DatabaseURL)
	if err != nil {
		return errors.New("DOCUMENTS_DATABASE_URL is invalid")
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return errors.New("DOCUMENTS_DATABASE_URL: cannot make the pool")
	}
	defer pool.Close()

	opts := []nats.Option{nats.Name("documents"), nats.CustomInboxPrefix(inboxPrefix), nats.MaxReconnects(-1),
		nats.Timeout(5 * time.Second)}
	if s.NATSNKeyFile != "" {
		o, err := nats.NkeyOptionFromSeed(s.NATSNKeyFile)
		if err != nil {
			return errors.New("DOCUMENTS_NATS_NKEY_FILE: cannot read the NKey seed")
		}
		opts = append(opts, o)
	} else {
		opts = append(opts, nats.UserInfo(natsUser, s.NATSPassword))
	}
	nc, err := nats.Connect(s.NATSURL, opts...)
	if err != nil {
		return errors.New("cannot connect to NATS")
	}
	defer nc.Close()

	r := &ctl.Responder{
		Keys:     func() map[string]ed25519.PublicKey { return keys },
		Replay:   replay.New(ctl.ReplayWindow, ctl.MaxReplayIDs),
		Handlers: map[string]ctl.Handler{"health": healthHandler(pool)},
		Log:      log,
		Metrics:  &ctl.Metrics{},
	}
	stopCtl, err := r.Serve(ctx, nc)
	if err != nil {
		return err
	}
	defer stopCtl()
	logStart(log, s.Env, len(keys))
	<-ctx.Done()
	return nil
}
