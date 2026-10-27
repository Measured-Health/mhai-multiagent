// Command core is the core service: the only service the BFFs talk to.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/config"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/logx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/password"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/server"
)

const usage = "usage: core serve | core migrate up|status | core hospitals create ... | core users create ..."

func main() {
	var err error
	switch {
	case len(os.Args) == 2 && os.Args[1] == "serve":
		err = serve()
	case len(os.Args) >= 2 && os.Args[1] == "migrate":
		err = migrate(context.Background(), os.Args[2:], os.Getenv("DATABASE_URL"), os.Stdout)
	case len(os.Args) >= 2 && (os.Args[1] == "hospitals" || os.Args[1] == "users"):
		err = seed(context.Background(), os.Args[1:], os.Getenv("DATABASE_URL"), os.Stdin, os.Stdout)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "core:", err)
		os.Exit(1)
	}
}

// migrate runs "migrate up" or "migrate status". It needs only DATABASE_URL,
// so the one-shot migrate container carries no core service secrets.
func migrate(ctx context.Context, args []string, url string, out io.Writer) error {
	if len(args) != 1 || (args[0] != "up" && args[0] != "status") {
		return fmt.Errorf("%s", usage)
	}
	if url == "" {
		return errors.New("DATABASE_URL is required")
	}
	if args[0] == "up" {
		if err := db.MigrateUp(ctx, url); err != nil {
			return err
		}
	}
	st, err := db.Status(ctx, url)
	if err != nil {
		return err
	}
	for _, s := range st {
		fmt.Fprintf(out, "%05d %-8s %s\n", s.Source.Version, s.State, s.Source.Path)
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
	pool, err := openDB(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           newHandler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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

// newHandler serves the health probe. Task 9 mounts the full route set
// (server.Register) with its dependencies.
func newHandler() http.Handler {
	m := authz.NewMux(nil)
	server.RegisterHealth(m)
	return m.Handler(nil)
}

// openDB connects and refuses to go on unless the login user is held by
// row-level security (see db.CheckLoginRole).
func openDB(ctx context.Context, url string) (*pgxpool.Pool, error) {
	if url == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	pool, err := db.Open(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := db.CheckLoginRole(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}
