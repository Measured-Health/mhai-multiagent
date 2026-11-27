package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/config"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/secrets"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/secretsmove"
)

// moveFromFile is "emr secrets move-from-file --dir <dir> --env <env>"
// (deploy/openbao/enable-openbao.sh): every credential the file store at
// dir holds for env (staging on the live host before the switch) is moved
// to the OpenBao store the service's settings name, under the service's
// own APP_ENV. Idempotent. Prints counts only.
func moveFromFile(getenv func(string) string, dir, fromEnv string, out io.Writer) error {
	s, err := config.FromEnv(getenv)
	if err != nil {
		return err
	}
	if s.SecretStore != "openbao" {
		return errors.New("emr secrets move-from-file: EMR_SECRET_STORE must be openbao (the destination)")
	}
	from, err := secrets.NewFileStore(dir, fromEnv)
	if err != nil {
		return errors.New("emr secrets move-from-file: --dir must be a file store (0700, this user's) and --env development or staging")
	}
	to, err := openStore(s)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := to.Ready(ctx); err != nil {
		return errors.New("emr secrets move-from-file: OpenBao is not ready (sealed, down, or these credentials refused)")
	}
	pool, err := pgxpool.New(ctx, s.DatabaseURL)
	if err != nil {
		return errors.New("EMR_DATABASE_URL: cannot make the pool")
	}
	defer pool.Close()
	r, err := secretsmove.FromFile(ctx, pool, from, to, s.Env)
	fmt.Fprintf(out, "moved %d, already in OpenBao %d, without a metadata row (left) %d, missing from the file store %d\n",
		r.Moved, r.Already, r.Orphan, r.Missing)
	return err
}

// secretsReady is "emr secrets ready": the store the settings name can be
// reached, is unsealed, and takes this service's credentials (the deploy
// and enable-openbao.sh ask it; health says the same over NATS). Exit 1
// when not, naming no path or value.
func secretsReady(getenv func(string) string, out io.Writer) error {
	s, err := config.FromEnv(getenv)
	if err != nil {
		return err
	}
	st, err := openStore(s)
	if err != nil {
		return err
	}
	if st == nil {
		return errors.New("emr secrets ready: no secret store is configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := st.Ready(ctx); err != nil {
		return fmt.Errorf("emr secrets ready: the %s store is not ready (sealed, down, or its credentials refused)", s.SecretStore)
	}
	fmt.Fprintf(out, "secret store ready (%s, %s)\n", s.SecretStore, s.Env)
	return nil
}
