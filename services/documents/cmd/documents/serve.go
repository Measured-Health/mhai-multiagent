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
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/dblogin"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/kek"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/s3"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/storage"
	"github.com/Measured-Health/mhai-multiagent/services/documents/internal/upload"
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

// checker reports one part's readiness: nil is ready.
type checker func(ctx context.Context) error

// readiness is what health reports and what an upload needs.
type readiness struct {
	db, keyring, storage checker
}

// ready runs c with a 2-second deadline; a missing checker is not ready.
func ready(ctx context.Context, c checker) bool {
	if c == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return c(ctx) == nil
}

// healthHandler answers documents.v1.ctl.health: readiness flags only, no
// counts of anyone's data. The database is ready when its login answers
// and holds no more than mhai_documents (departure 10); the keyring when
// every KEK passes its test unwrap; storage when the bucket's lifecycle is
// in force. The scanner and the sandbox are PR 4's, so they are reported
// as not ready and the service never claims it can take a document
// (design §17 departure 4).
func healthHandler(r readiness) ctl.Handler {
	return func(ctx context.Context, _ reqtoken.Claims, _ proto.Message) (proto.Message, error) {
		return &docspb.HealthReply{DatabaseReady: ready(ctx, r.db), KeyringReady: ready(ctx, r.keyring),
			StorageReady: ready(ctx, r.storage)}, nil
	}
}

// uploadReady is what presign_upload needs: the keyring and the store.
func uploadReady(r readiness) func(context.Context) error {
	return func(ctx context.Context) error {
		for _, c := range []checker{r.keyring, r.storage} {
			if c == nil {
				return errNotReady
			}
			if err := c(ctx); err != nil {
				return err
			}
		}
		return nil
	}
}

var errNotReady = errors.New("documents: not ready")

// newLogger is the service's logger: JSON, with gokit's redaction of
// credential and personal-data keys.
func newLogger(w io.Writer) *slog.Logger { return logx.New(w) }

// logStart logs the start with settings' shapes only, never their values.
func logStart(log *slog.Logger, env string, trustedKeys int, provider string, keyringReady, storageReady bool) {
	log.Info("documents serving", "env", env, "subjects", ctl.Subjects, "trusted_keys", trustedKeys,
		"kek_provider", provider, "keyring_ready", keyringReady, "storage_ready", storageReady,
		"scanner_ready", false, "sandbox_ready", false)
}

// keyringReason is a keyring error's closed code for the log.
func keyringReason(err error) string {
	for _, c := range []struct {
		err  error
		code string
	}{{kek.ErrKeyringMissing, "missing"}, {kek.ErrKeyringMode, "mode"}, {kek.ErrKeyringPlaceholder, "placeholder"},
		{kek.ErrKeyringCheck, "check"}, {kek.ErrKeyringInvalid, "invalid"}} {
		if errors.Is(err, c.err) {
			return c.code
		}
	}
	return "error"
}

// openKeyring is the configured KEK provider, or nil. The file provider
// is announced with a warning at every start (design §6.2), and a keyring
// that fails its check leaves the service not ready, not stopped.
func openKeyring(ctx context.Context, s config.Settings, log *slog.Logger) kek.Provider {
	if s.KEKProvider != "file" {
		log.Warn("documents: no key-encryption key provider; the keyring is not ready")
		return nil
	}
	log.Warn("documents: the file keyring keeps every hospital's KEK on this host; pilot and development only",
		"accepted_for_pilot", s.FileKeyringAccepted())
	f := kek.NewFile(s.KeyringPath)
	if err := f.Check(ctx); err != nil {
		log.Warn("documents: the keyring is not ready", "reason", keyringReason(err))
	}
	return f
}

// openStore is the object store client, or nil when none is configured.
// A secret file that is missing, short or a placeholder stops the service.
func openStore(s config.Settings) (*s3.Client, error) {
	if s.S3 == nil {
		return nil, nil
	}
	secret, err := config.ReadSecretFile("DOCUMENTS_S3_SECRET_KEY_FILE", s.S3.SecretKeyFile)
	if err != nil {
		return nil, err
	}
	c, err := s3.New(s3.Config{Endpoint: s.S3.Endpoint, Bucket: s.S3.Bucket, Region: s.S3.Region,
		Credentials: s3.Credentials{AccessKeyID: s.S3.AccessKeyID, Secret: secret}})
	if err != nil {
		return nil, errors.New("DOCUMENTS_S3_ settings are invalid")
	}
	return c, nil
}

// checkLogin refuses a database login that can do more than mhai_documents
// (departure 10). A database that doesn't answer yet is not a refusal: the
// pool connects lazily and health reports database_ready false.
func checkLogin(ctx context.Context, q dblogin.Querier) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := dblogin.Check(ctx, q)
	var pe *dblogin.PrivilegedError
	if errors.As(err, &pe) {
		return errors.New("DOCUMENTS_DATABASE_URL: the database login is too privileged (" + pe.Reason + ")")
	}
	return nil
}

func serve(getenv func(string) string) error {
	s, err := config.FromEnv(getenv)
	if err != nil {
		return err
	}
	log := newLogger(os.Stdout)
	trusted, err := reqtoken.ReadTrusted(s.TrustedKeysPath)
	if err != nil {
		return errors.New("DOCUMENTS_TRUSTED_KEYS_PATH: the trusted request-keys file is missing or not valid")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	keys := openKeyring(ctx, s, log)
	store, err := openStore(s)
	if err != nil {
		return err
	}

	// The pool connects lazily: the service starts (and health answers
	// database_ready false) while the database or its login isn't there.
	cfg, err := pgxpool.ParseConfig(s.DatabaseURL)
	if err != nil {
		return errors.New("DOCUMENTS_DATABASE_URL is invalid")
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return errors.New("DOCUMENTS_DATABASE_URL: cannot make the pool")
	}
	defer pool.Close()
	if err := checkLogin(ctx, pool); err != nil {
		return err
	}

	rd := readiness{db: func(ctx context.Context) error { return dblogin.Check(ctx, pool) }}
	if keys != nil {
		rd.keyring = keys.Check
	}
	presign := &upload.Presign{Rows: upload.PGRows{Pool: pool}}
	if store != nil {
		rd.storage = (&storage.Readiness{Objects: store}).Check
		presign.Signer, presign.UploadEndpoint = store, s.S3.UploadEndpoint
	}
	presign.Ready = uploadReady(rd)

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
		Keys:   func() map[string]ed25519.PublicKey { return trusted },
		Replay: replay.New(ctl.ReplayWindow, ctl.MaxReplayIDs),
		Handlers: map[string]ctl.Handler{
			"health":         healthHandler(rd),
			"presign_upload": presign.Handle,
		},
		Log:     log,
		Metrics: &ctl.Metrics{},
	}
	stopCtl, err := r.Serve(ctx, nc)
	if err != nil {
		return err
	}
	defer stopCtl()
	provider := "none"
	if keys != nil {
		provider = keys.Name()
	}
	logStart(log, s.Env, len(trusted), provider, ready(ctx, rd.keyring), ready(ctx, rd.storage))
	<-ctx.Done()
	return nil
}
