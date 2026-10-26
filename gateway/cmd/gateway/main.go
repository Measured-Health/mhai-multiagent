// Command gateway is the Go gateway: the only service the BFFs talk to.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Measured-Health/mhai-multiagent/gateway/internal/config"
	"github.com/Measured-Health/mhai-multiagent/gateway/internal/db"
	"github.com/Measured-Health/mhai-multiagent/gateway/internal/logx"
)

const usage = "usage: gateway serve | gateway migrate up|status"

func main() {
	var err error
	switch {
	case len(os.Args) == 2 && os.Args[1] == "serve":
		err = serve()
	case len(os.Args) >= 2 && os.Args[1] == "migrate":
		err = migrate(context.Background(), os.Args[2:], os.Getenv("DATABASE_URL"), os.Stdout)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
		os.Exit(1)
	}
}

// migrate runs "migrate up" or "migrate status". It needs only DATABASE_URL,
// so the one-shot migrate container carries no gateway secrets.
func migrate(ctx context.Context, args []string, url string, out io.Writer) error {
	if len(args) != 1 || (args[0] != "up" && args[0] != "status") {
		return errors.New(usage)
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
	log := logx.New(os.Stdout)
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
	log.Info("gateway listening", "addr", cfg.HTTPAddr, "env", cfg.Env)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func newHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	return requestID(mux)
}

// requestID puts an X-Request-ID on every response.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		w.Header().Set("X-Request-ID", hex.EncodeToString(b))
		next.ServeHTTP(w, r)
	})
}
