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
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/logx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/notifypb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/replay"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/channel"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/config"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/ctl"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/dblogin"
	"github.com/Measured-Health/mhai-multiagent/services/notifications/internal/deliver"
)

// natsUser is the notifications service's NATS user; inboxPrefix its own
// inbox (services/nats/notifications.conf).
const (
	natsUser    = "notifications"
	inboxPrefix = "_INBOX_worker.notifications"
)

// checker reports one part's readiness: nil is ready.
type checker func(ctx context.Context) error

// ready runs c with a 2-second deadline; a missing checker is not ready.
func ready(ctx context.Context, c checker) bool {
	if c == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return c(ctx) == nil
}

// healthHandler answers notify.v1.ctl.health: readiness flags and the
// enabled channels' names, no counts of anyone's messages. The database is
// ready when its login answers and holds no more than mhai_notify.
func healthHandler(db checker, channels []string) ctl.Handler {
	return func(ctx context.Context, _ reqtoken.Claims, _ proto.Message) (proto.Message, error) {
		return &notifypb.HealthReply{DatabaseReady: ready(ctx, db), Channels: append([]string(nil), channels...)}, nil
	}
}

// newLogger is the service's logger: JSON, with gokit's redaction of
// credential and personal-data keys.
func newLogger(w io.Writer) *slog.Logger { return logx.New(w) }

// logStart logs the start with settings' shapes only, never their values.
func logStart(log *slog.Logger, env string, trustedKeys int, channels []string, dbReady bool) {
	log.Info("notifications serving", "env", env, "subjects", ctl.Subjects, "trusted_keys", trustedKeys,
		"channels", channels, "sends", len(channels) > 0, "database_ready", dbReady)
	if len(channels) == 0 {
		log.Warn("notifications: NOTIFY_CHANNELS is empty; nothing is sent")
	}
}

// channels builds the enabled channels (config allows log in development
// only; email comes with PR 4).
func channels(s config.Settings, log *slog.Logger) map[string]channel.Channel {
	out := map[string]channel.Channel{}
	for _, name := range s.Channels {
		if name == config.ChannelLog {
			out[name] = channel.Log{Logger: log}
		}
	}
	return out
}

// newSender is the notify_send handler from the settings.
func newSender(s config.Settings, st deliver.Store, dead deliver.DeadLetters, log *slog.Logger) *deliver.Sender {
	return &deliver.Sender{Store: st, Channels: channels(s, log), Dead: dead, Log: log, Metrics: &deliver.Metrics{},
		Limits:  deliver.Limits{Recipient: s.RecipientPerHour, Hospital: s.HospitalPerHour},
		Pacer:   deliver.NewPacer(s.RatePerSecond),
		Breaker: deliver.NewBreaker()}
}

// checkLogin refuses a database login that can do more than mhai_notify. A
// database that doesn't answer yet is not a refusal: the pool connects
// lazily and health reports database_ready false.
func checkLogin(ctx context.Context, q dblogin.Querier) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := dblogin.Check(ctx, q)
	var pe *dblogin.PrivilegedError
	if errors.As(err, &pe) {
		return errors.New("NOTIFY_DATABASE_URL: the database login is too privileged (" + pe.Reason + ")")
	}
	return nil
}

// natsOptions are the connection's options: its own name and inbox, and
// its credential (an NKey seed file, or the development password).
func natsOptions(s config.Settings) ([]nats.Option, error) {
	opts := []nats.Option{nats.Name("notifications"), nats.CustomInboxPrefix(inboxPrefix), nats.MaxReconnects(-1),
		nats.Timeout(5 * time.Second)}
	if s.NATSNKeyFile != "" {
		o, err := nats.NkeyOptionFromSeed(s.NATSNKeyFile)
		if err != nil {
			return nil, errors.New("NOTIFY_NATS_NKEY_FILE: cannot read the NKey seed")
		}
		return append(opts, o), nil
	}
	return append(opts, nats.UserInfo(natsUser, s.NATSPassword)), nil
}

func serve(getenv func(string) string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveUntil(ctx, getenv, os.Stdout, nil)
}

// serveUntil runs the service until ctx ends, logging to w. started, if
// set, is called once the responder is subscribed.
func serveUntil(ctx context.Context, getenv func(string) string, w io.Writer, started func()) error {
	s, err := config.FromEnv(getenv)
	if err != nil {
		return err
	}
	log := newLogger(w)
	keys, err := reqtoken.ReadTrusted(s.TrustedKeysPath)
	if err != nil {
		return errors.New("NOTIFY_TRUSTED_KEYS_PATH: the trusted request-keys file is missing or not valid")
	}

	// The pool connects lazily: the service starts (and health answers
	// database_ready false) while the database or its login isn't there.
	cfg, err := pgxpool.ParseConfig(s.DatabaseURL)
	if err != nil {
		return errors.New("NOTIFY_DATABASE_URL is invalid")
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return errors.New("NOTIFY_DATABASE_URL: cannot make the pool")
	}
	defer pool.Close()
	if err := checkLogin(ctx, pool); err != nil {
		return err
	}
	db := func(ctx context.Context) error { return dblogin.Check(ctx, pool) }

	opts, err := natsOptions(s)
	if err != nil {
		return err
	}
	nc, err := nats.Connect(s.NATSURL, opts...)
	if err != nil {
		return errors.New("cannot connect to NATS")
	}
	defer nc.Close()

	r := &ctl.Responder{
		Keys:     func() map[string]ed25519.PublicKey { return keys },
		Replay:   replay.New(ctl.ReplayWindow, ctl.MaxReplayIDs),
		Handlers: map[string]ctl.Handler{"health": healthHandler(db, s.Channels)},
		Log:      log,
		Metrics:  &ctl.Metrics{},
	}
	stopCtl, err := r.Serve(ctx, nc)
	if err != nil {
		return err
	}
	defer stopCtl()
	// The sender (PR 3): only with a channel on. It waits for the NOTIFY
	// stream, which core makes only with CORE_NOTIFY_ENABLED.
	if s.Sends() {
		js, err := jetstream.New(nc)
		if err != nil {
			return errors.New("cannot open JetStream")
		}
		st := deliver.PG{Pool: pool}
		go newSender(s, st, deliver.JSDead{JS: js}, log).Run(ctx, js)
		go deliver.RunRetention(ctx, st, log)
	}
	logStart(log, s.Env, len(keys), s.Channels, ready(ctx, db))
	if started != nil {
		started()
	}
	<-ctx.Done()
	return nil
}
