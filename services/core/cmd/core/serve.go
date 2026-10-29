package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	core "github.com/Measured-Health/mhai-multiagent/services/core"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/apidocs"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/config"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/grant"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/intel"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/logx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/natsapi"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/password"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/server"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/telemetry"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

// SweepEvery is how often serve deletes expired Idempotency-Key rows.
const SweepEvery = 10 * time.Minute

// app is the running service's parts.
type app struct {
	handler http.Handler
	pool    *pgxpool.Pool
	nc      *nats.Conn
	intel   *intel.Client
	js      jetstream.JetStream
	log     *slog.Logger
	// stopAPI ends core's NATS API (natsapi.Serve) and stopGrant its grant
	// endpoint (grant.Server) before NATS closes.
	stopAPI, stopGrant func()
}

func (a *app) close() {
	if a.stopGrant != nil {
		a.stopGrant()
	}
	if a.stopAPI != nil {
		a.stopAPI()
	}
	if a.nc != nil {
		a.nc.Close()
	}
	if a.pool != nil {
		a.pool.Close()
	}
}

// newApp connects everything serve needs and assembles the routes:
//   - Postgres as a login that is only a member of mhai_app (anything else,
//     the owner or the provisioning login included, is refused);
//   - NATS with the core service's own credentials, inbox prefix and TLS
//     files, the KV buckets, and the JOBS and DEAD streams with their
//     consumers;
//   - the access-token key, the auth service and the principal middleware
//     with both idempotency stores (authz.NewMux refuses a guard without);
//   - the request-token key (a different key from the access-token key)
//     and the intelligence client on core's own connection;
//   - the frontend contract over NATS (natsapi.Serve on api.v1.clinical and
//     api.v1.platform, queue group core) always, and over HTTP with the BFF
//     key only while CORE_HTTP_API is on. Off, HTTP serves the probes only;
//   - the grant endpoint (grant.Server on authz.v1.grant.>, queue group
//     core) with the same guard, so a BFF-direct call is checked by the
//     principal middleware's own code. Test-only operations are granted
//     only in development.
func newApp(ctx context.Context, cfg config.Config, log *slog.Logger) (*app, error) {
	if err := cfg.ValidateServe(); err != nil {
		return nil, err
	}
	issuer, err := token.LoadIssuer(cfg.AccessKeyPath)
	if err != nil {
		return nil, err
	}
	signer, err := reqtoken.LoadSigner(cfg.RequestKeyPath)
	if err != nil {
		return nil, err
	}
	if err := checkSigningKeys(issuer, signer); err != nil {
		return nil, err
	}
	a := &app{log: log}
	if a.pool, err = openDB(ctx, cfg.DatabaseURL); err != nil {
		return nil, err
	}
	// The link flips /readyz while the connection is down (TODO T3 M6).
	link := bus.NewLink(log)
	nc, js, err := bus.Connect(ctx, cfg.NATSURL, cfg.NATSNkeySeedPath, bus.GatewayInboxPrefix, bus.TLS(cfg.NATSTLS),
		link.Options()...)
	if err != nil {
		a.close()
		return nil, err
	}
	a.nc = nc
	buckets, err := bus.OpenBuckets(ctx, js, cfg.NATSJSReplicas)
	if err != nil {
		a.close()
		return nil, err
	}
	if err := bus.EnsureStreams(ctx, js, bus.StreamConfig{
		Replicas: cfg.NATSJSReplicas, PipelineMaxAckPending: cfg.JobsPipelineMaxAckPending,
	}); err != nil {
		a.close()
		return nil, err
	}
	a.intel = &intel.Client{NC: nc, JS: js, Signer: signer, Log: log, Metrics: &intel.Metrics{}}
	a.js = js
	if cfg.RequestTrustedKeysPath != "" {
		// The workers' public file: a kid missing from it is one an
		// emergency rotation removed (dead-letter re-mint, spec §7.5).
		if a.intel.Trusted, err = intel.LoadTrustedKeys(cfg.RequestTrustedKeysPath); err != nil {
			a.close()
			return nil, errors.New("REQUEST_TRUSTED_KEYS_PATH: the trusted request-keys file is missing or not valid")
		}
		if !a.intel.Trusted.Has(signer.KeyID) {
			log.Warn("core: the request-signing kid is not in the trusted request-keys file; workers will refuse its tokens",
				"kid", signer.KeyID)
		}
	}
	keys := issuer.PublicKeys()
	svc := &auth.Service{
		Pool: a.pool, Buckets: buckets, Issuer: issuer, Keys: keys, Log: log,
		MFAKey: cfg.MFAKey, Argon: password.DefaultGate,
	}
	guard := &authz.Guard{
		Keys: keys, Denylist: buckets.Denylist, Pool: a.pool, Log: log,
		Idem:     httpx.IdempotentLog(a.pool, log),
		IdemAnon: httpx.IdempotentAnonLog(a.pool, log),
	}
	m := authz.NewMux(guard)
	server.Register(m, server.Deps{Auth: svc, Pool: a.pool, Ready: server.WithLink(server.Readiness(a.pool, nc, buckets.All()), link),
		Intel: a.intel, QueryCap: cfg.IntelligenceQueryCap})
	// The same chain over NATS, without the BFF key (the bff_<app> NATS
	// user is the credential) and without the probes or the docs.
	apiMetrics := &natsapi.Metrics{}
	if a.stopAPI, err = natsapi.Serve(ctx, nc, m.NATSHandler(), log, natsapi.WithConcurrency(cfg.NATSAPIConcurrency),
		natsapi.WithMetrics(apiMetrics)); err != nil {
		a.close()
		return nil, err
	}
	grantMetrics := &grant.Metrics{}
	grants := &grant.Server{Guard: guard, Signer: signer, Pool: a.pool, Log: log, Metrics: grantMetrics,
		AllowTestOnly: cfg.Development()}
	if a.stopGrant, err = grants.Serve(ctx, nc); err != nil {
		a.close()
		return nil, err
	}
	registerMetrics(a, apiMetrics, grantMetrics)
	if !cfg.HTTPAPI {
		a.handler = m.ProbesHandler()
		return a, nil
	}
	// The docs routes sit outside the API chain and are never served over
	// NATS (see internal/apidocs): only this HTTP handler carries them.
	a.handler = apidocs.Wrap(cfg.APIDocs, core.OpenAPI, m.Handler(cfg.BFFServiceKeys))
	return a, nil
}

// checkSigningKeys refuses one key for both token kinds (spec §7.2): the
// files must differ (config.ValidateServe) and so must the keys, so a
// copied key can't turn one token kind into the other. Key ids are the
// key's hash, so equal kids mean the same key.
func checkSigningKeys(access token.Issuer, request reqtoken.Signer) error {
	if access.KeyID == request.KeyID || access.Key.Equal(request.Key) {
		return errors.New("REQUEST_KEY_PATH holds the same key as ACCESS_KEY_PATH; make a separate request-token key (core keys new --kind request)")
	}
	return nil
}

// runSweeper deletes expired Idempotency-Key rows now and then every
// interval until ctx ends. A failed sweep is logged and retried next tick.
func runSweeper(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		n, err := db.SweepIdempotency(ctx, pool)
		if err != nil && ctx.Err() == nil {
			log.Error("core: idempotency sweep failed")
		} else if n > 0 {
			log.Info("core: idempotency sweep", "deleted", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// reloadTrustedOnHUP re-reads t on every SIGHUP until ctx ends; a failed
// read keeps the last good set.
func reloadTrustedOnHUP(ctx context.Context, t *intel.TrustedKeys, log *slog.Logger) {
	if t == nil {
		return
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			if err := t.Reload(); err != nil {
				log.Error("core: trusted request-keys reload failed; keeping the last set")
			} else {
				log.Info("core: trusted request-keys reloaded")
			}
		}
	}
}

// telemetryWarnings are the start-up warnings about the telemetry
// settings: OTEL_EXPORTER_OTLP_INSECURE outside development sends spans
// and metrics in plaintext to a non-loopback collector.
func telemetryWarnings(cfg config.Config) []string {
	if cfg.OTLPEndpoint != "" && cfg.OTLPInsecure && !cfg.Development() {
		return []string{"core: OTEL_EXPORTER_OTLP_INSECURE is on outside development: traces and metrics may leave the host in plaintext"}
	}
	return nil
}

func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := password.Preload(); err != nil {
		return err
	}
	log := logx.New(os.Stdout)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Telemetry is off unless OTEL_EXPORTER_OTLP_ENDPOINT is set, and a bad
	// endpoint turns it off with a warning: observability never keeps the
	// service from starting.
	for _, w := range telemetryWarnings(cfg) {
		log.Warn(w)
	}
	shutdownTelemetry, terr := telemetry.Setup(ctx, cfg.OTLPEndpoint, telemetry.AllowPlaintext(cfg.OTLPInsecure || cfg.Development()))
	if terr != nil {
		// The reason is one of telemetry's fixed messages (a plaintext
		// endpoint refused, say); it never carries the endpoint itself.
		log.Warn("core: telemetry is off: OTEL_EXPORTER_OTLP_ENDPOINT is not usable", "reason", terr.Error())
	} else {
		defer func() {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = shutdownTelemetry(c)
		}()
	}
	a, err := newApp(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer a.close()
	go runSweeper(ctx, a.pool, log, SweepEvery)
	// Republishes queued questions whose publish after commit was lost.
	go intel.RunPublishSweeper(ctx, a.pool, a.intel, intel.SweepEvery)
	// Moves the workers' job facts (intelligence_audit_outbox) onto the chain.
	go intel.RunDrainer(ctx, a.pool, intel.DrainEvery, log)
	// Marks dead-lettered jobs failed, or re-mints them after an emergency
	// key rotation (core_dead, shared by every core copy).
	go func() {
		if err := intel.RunDeadLetters(ctx, a.pool, a.js, a.intel); err != nil {
			log.Error("core: dead-letter consumer stopped", "error", err)
		}
	}()
	// SIGHUP re-reads the trusted request-keys file (after keys untrust).
	go reloadTrustedOnHUP(ctx, a.intel.Trusted, log)
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           a.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("core listening", "addr", cfg.HTTPAddr, "env", cfg.Env, "http_api", cfg.HTTPAPI,
		"api_docs", cfg.APIDocs && cfg.HTTPAPI, "nats_api", natsapi.SubjectClinical+","+natsapi.SubjectPlatform, "grants", grant.Subject)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
