// Command core is the core service: the only service the BFFs talk to.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/config"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
)

const usage = "usage: core serve | core migrate up|status | core keys new --out PATH | core hospitals create ... | core users create ..."

func main() {
	var err error
	switch {
	case len(os.Args) == 2 && os.Args[1] == "serve":
		err = serve()
	case len(os.Args) >= 2 && os.Args[1] == "migrate":
		err = migrateWithLogins(context.Background(), os.Args[2:], os.Getenv("DATABASE_URL"), os.Getenv, os.Stdout)
	case len(os.Args) >= 2 && os.Args[1] == "keys":
		err = keys(os.Args[2:], os.Stdout)
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

// loginRequest is a deployment login that migrate up creates or resets.
type loginRequest struct {
	userVar, pwVar string
	user, password string
	role           string
}

// loginRequests reads the logins to ensure from the environment:
//   - CORE_DB_USER / CORE_DB_PASSWORD: the core service's login, a member
//     of mhai_app only;
//   - PROVISION_DB_USER / PROVISION_DB_PASSWORD: the seed CLI's login, a
//     member of mhai_provision only.
//
// Each pair is both set or both unset. Passwords must not be placeholders.
// Errors name variables, never values.
func loginRequests(getenv func(string) string) ([]loginRequest, error) {
	var out []loginRequest
	var errs []error
	for _, l := range []loginRequest{
		{userVar: "CORE_DB_USER", pwVar: "CORE_DB_PASSWORD", role: "mhai_app"},
		{userVar: "PROVISION_DB_USER", pwVar: "PROVISION_DB_PASSWORD", role: "mhai_provision"},
	} {
		l.user, l.password = getenv(l.userVar), getenv(l.pwVar)
		switch {
		case l.user == "" && l.password == "":
			continue
		case l.user == "":
			errs = append(errs, fmt.Errorf("%s is required with %s", l.userVar, l.pwVar))
			continue
		}
		if err := config.CheckPassword(l.pwVar, l.password); err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, l)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

// migrateWithLogins is "core migrate": the migrations, then, after "up",
// the deployment logins named in the environment (loginRequests). The
// passwords are checked before the database is touched.
func migrateWithLogins(ctx context.Context, args []string, url string, getenv func(string) string, out io.Writer) error {
	reqs, err := loginRequests(getenv)
	if err != nil {
		return err
	}
	if err := migrate(ctx, args, url, out); err != nil {
		return err
	}
	if len(reqs) == 0 || args[0] != "up" {
		return nil
	}
	pool, err := db.Open(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	for _, l := range reqs {
		if err := db.EnsureLogin(ctx, pool, l.user, l.password, l.role); err != nil {
			return err
		}
		fmt.Fprintf(out, "login %s ready (member of %s only)\n", l.user, l.role)
	}
	return nil
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
