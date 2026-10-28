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

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/config"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/logx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/password"
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
	log     *slog.Logger
}

func (a *app) close() {
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
//   - NATS with the core service's own credentials and inbox prefix, and
//     the KV buckets;
//   - the access-token key, the auth service and the principal middleware
//     with both idempotency stores (authz.NewMux refuses a guard without).
func newApp(ctx context.Context, cfg config.Config, log *slog.Logger) (*app, error) {
	if err := cfg.ValidateServe(); err != nil {
		return nil, err
	}
	issuer, err := token.LoadIssuer(cfg.AccessKeyPath)
	if err != nil {
		return nil, err
	}
	a := &app{log: log}
	if a.pool, err = openDB(ctx, cfg.DatabaseURL); err != nil {
		return nil, err
	}
	nc, js, err := bus.Connect(ctx, cfg.NATSURL, cfg.NATSNkeySeedPath, bus.GatewayInboxPrefix)
	if err != nil {
		a.close()
		return nil, err
	}
	a.nc = nc
	buckets, err := bus.OpenBuckets(ctx, js)
	if err != nil {
		a.close()
		return nil, err
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
	server.Register(m, server.Deps{Auth: svc, Pool: a.pool, Ready: server.Readiness(a.pool, nc, buckets.All())})
	a.handler = m.Handler(cfg.BFFServiceKeys)
	return a, nil
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
	log.Info("core listening", "addr", cfg.HTTPAddr, "env", cfg.Env)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
