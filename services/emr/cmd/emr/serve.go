package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/config"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/ctl"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/logx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/replay"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

var errUsage = errors.New("usage")

// natsUser is the emr service's NATS user; InboxPrefix its own inbox
// (services/nats/nats.conf).
const (
	natsUser    = "emr"
	InboxPrefix = "_INBOX_worker.emr"
)

// pinger is what health needs of the database.
type pinger interface {
	Ping(ctx context.Context) error
}

// healthHandler answers emr.v1.ctl.health: readiness flags only, no
// counts of anyone's data. Nothing is stored or connected yet in this
// skeleton, so the secret store and the rule set are reported as absent
// and the proxy allow-list as never written.
func healthHandler(db pinger) ctl.Handler {
	return func(ctx context.Context, _ reqtoken.Claims, _ proto.Message) (proto.Message, error) {
		ready := false
		if db != nil {
			c, cancel := context.WithTimeout(ctx, 2*time.Second)
			ready = db.Ping(c) == nil
			cancel()
		}
		return &emrpb.HealthReply{DatabaseReady: ready, SecretStoreReady: false, RuleSetVersion: "",
			ProxyAllowListAgeS: -1}, nil
	}
}

func serve(getenv func(string) string) error {
	s, err := config.FromEnv(getenv)
	if err != nil {
		return err
	}
	log := logx.New(os.Stdout)
	keys, err := reqtoken.ReadTrusted(s.TrustedKeysPath)
	if err != nil {
		return errors.New("EMR_TRUSTED_KEYS_PATH: the trusted request-keys file is missing or not valid")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The pool connects lazily: the service starts (and health answers
	// database_ready false) while the database or its login is not there.
	cfg, err := pgxpool.ParseConfig(s.DatabaseURL)
	if err != nil {
		return errors.New("EMR_DATABASE_URL is invalid")
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return errors.New("EMR_DATABASE_URL: cannot make the pool")
	}
	defer pool.Close()

	opts := []nats.Option{nats.Name("emr"), nats.CustomInboxPrefix(InboxPrefix), nats.MaxReconnects(-1)}
	if s.NATSNKeyFile != "" {
		o, err := nats.NkeyOptionFromSeed(s.NATSNKeyFile)
		if err != nil {
			return errors.New("EMR_NATS_NKEY_FILE: cannot read the NKey seed")
		}
		opts = append(opts, o)
	} else {
		opts = append(opts, nats.UserInfo(natsUser, s.NATSPassword))
	}
	nc, err := nats.Connect(s.NATSURL, opts...)
	if err != nil {
		return errors.New("emr: cannot connect to NATS")
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
	log.Info("emr serving", "env", s.Env, "subjects", ctl.Subjects, "rule_set_active", s.RuleSetActive != "")
	<-ctx.Done()
	return nil
}
