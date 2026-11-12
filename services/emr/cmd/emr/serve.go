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
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/codes"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/config"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector/stub"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/ctl"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/egress"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/normalise"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/proxyconf"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/secrets"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/logx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/replay"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
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

// readier is what health needs of the secret store.
type readier interface {
	Ready(ctx context.Context) error
}

// healthHandler answers emr.v1.ctl.health: readiness flags only, no
// counts of anyone's data. No rule set is active yet, so it is reported as
// absent. The allow-list's age is in whole seconds, -1 when this process
// hasn't written one (no egress configured, or no write yet).
func healthHandler(db pinger, store readier, allowAge func(time.Time) (time.Duration, bool)) ctl.Handler {
	return func(ctx context.Context, _ reqtoken.Claims, _ proto.Message) (proto.Message, error) {
		age := int64(-1)
		if allowAge != nil {
			if a, ok := allowAge(time.Now()); ok {
				age = int64(a / time.Second)
			}
		}
		ready, storeReady := false, false
		c, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if db != nil {
			ready = db.Ping(c) == nil
		}
		if store != nil {
			storeReady = store.Ready(c) == nil
		}
		return &emrpb.HealthReply{DatabaseReady: ready, SecretStoreReady: storeReady, RuleSetVersion: "",
			ProxyAllowListAgeS: age}, nil
	}
}

// loadSealKeys reads the sealing private keys (EMR_SEAL_KEY_PATH): the
// service refuses to start without one (design §17.4).
func loadSealKeys(dir string) (seal.Keys, error) {
	keys, err := seal.LoadPrivateDir(dir)
	if err != nil {
		return nil, errors.New("EMR_SEAL_KEY_PATH: no usable sealing key (emr keys seal-init)")
	}
	return keys, nil
}

// openStore opens the secret store: the file store at EMR_SECRETS_DIR, never
// in production (the managed store, plan Task 12, waits on H3). With no
// directory there is no store: the credential ops answer nothing (core
// shows not_ready) and health reports the store not ready.
func openStore(dir, env string) (*secrets.FileStore, error) {
	if dir == "" {
		return nil, nil
	}
	s, err := secrets.NewFileStore(dir, env)
	if err != nil {
		return nil, errors.New("EMR_SECRETS_DIR: the file store is for development and staging, in a 0700 directory of this user")
	}
	return s, nil
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
	sealKeys, err := loadSealKeys(s.SealKeyPath)
	if err != nil {
		return err
	}
	store, err := openStore(s.SecretsDir, s.Env)
	if err != nil {
		return err
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

	// The way out (PR 4): egress-emr and the allow-list it reads. Without
	// it there is none: the stub needs no network, and a connector that
	// does finds no proxy and fails closed.
	var writer *egress.Writer
	var allowAge func(time.Time) (time.Duration, bool)
	if s.ProxyURL != "" {
		if _, err := proxyconf.ReadKey(s.ProxyKeyPath); err != nil {
			return errors.New("EMR_PROXY_KEY_PATH: K_proxy missing or not a 32-byte hex key")
		}
		if st, err := os.Stat(s.EgressDir); err != nil || !st.IsDir() {
			return errors.New("EMR_EGRESS_DIR must be an existing directory")
		}
		writer = &egress.Writer{Pool: pool, Dir: s.EgressDir, Development: s.Development(), Log: log}
		allowAge = writer.Age
		go func() { _ = writer.Run(ctx) }()
	}

	// The normaliser (PR 5): only with code tables and maps configured.
	// Synthetic tables and unreviewed conversions load in development only.
	normaliser := false
	if s.CodesDir != "" {
		tables, err := codes.Load(s.CodesDir, s.Development())
		if err != nil {
			return errors.New("EMR_CODES_DIR: the code tables are missing, not valid, or synthetic outside development")
		}
		maps, err := codes.LoadMaps(s.MapsDir, tables, s.Development())
		if err != nil {
			return errors.New("EMR_MAPS_DIR: the maps are missing, not valid, or unreviewed outside development")
		}
		js, err := jetstream.New(nc)
		if err != nil {
			return errors.New("emr: cannot open JetStream")
		}
		n := &normalise.Normaliser{Pool: pool, Tables: tables, Maps: maps, Pub: normalise.JSPublisher{JS: js}, Log: log}
		go func() { _ = n.Run(ctx, js) }()
		go n.RunSweep(ctx)
		normaliser = true
	}

	handlers := map[string]ctl.Handler{}
	if store != nil {
		creds := &ctl.Credentials{Pool: pool, Store: store, Keys: sealKeys, Env: s.Env,
			Connectors: connector.NewRegistry(s.Development(), stub.New())}
		if writer != nil {
			creds.Egress = writer
		}
		handlers = creds.Handlers()
		handlers["health"] = healthHandler(pool, store, allowAge)
	} else {
		handlers["health"] = healthHandler(pool, nil, allowAge)
	}
	r := &ctl.Responder{
		Keys:     func() map[string]ed25519.PublicKey { return keys },
		Replay:   replay.New(ctl.ReplayWindow, ctl.MaxReplayIDs),
		Handlers: handlers,
		Log:      log,
		Metrics:  &ctl.Metrics{},
	}
	stopCtl, err := r.Serve(ctx, nc)
	if err != nil {
		return err
	}
	defer stopCtl()
	log.Info("emr serving", "env", s.Env, "subjects", ctl.Subjects, "rule_set_active", s.RuleSetActive != "",
		"secret_store", store != nil, "sealing_keys", len(sealKeys), "egress", writer != nil, "normaliser", normaliser)
	<-ctx.Done()
	return nil
}
