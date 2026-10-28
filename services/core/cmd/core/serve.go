package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

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
	nc, js, err := bus.Connect(ctx, cfg.NATSURL, cfg.NATSNkeySeedPath, bus.GatewayInboxPrefix, bus.TLS(cfg.NATSTLS))
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
	a.intel = &intel.Client{NC: nc, JS: js, Signer: signer, Log: log}
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
	server.Register(m, server.Deps{Auth: svc, Pool: a.pool, Ready: server.Readiness(a.pool, nc, buckets.All()),
		Intel: a.intel, QueryCap: cfg.IntelligenceQueryCap})
	// The same chain over NATS, without the BFF key (the bff_<app> NATS
	// user is the credential) and without the probes or the docs.
	if a.stopAPI, err = natsapi.Serve(ctx, nc, m.NATSHandler(), log, natsapi.WithConcurrency(cfg.NATSAPIConcurrency)); err != nil {
		a.close()
		return nil, err
	}
	grants := &grant.Server{Guard: guard, Signer: signer, Pool: a.pool, Log: log,
		AllowTestOnly: strings.EqualFold(strings.TrimSpace(cfg.Env), "development")}
	if a.stopGrant, err = grants.Serve(ctx, nc); err != nil {
		a.close()
		return nil, err
	}
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
	a, err := newApp(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer a.close()
	go runSweeper(ctx, a.pool, log, SweepEvery)
	// Republishes queued questions whose publish after commit was lost.
	go intel.RunPublishSweeper(ctx, a.pool, a.intel, intel.SweepEvery)
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
