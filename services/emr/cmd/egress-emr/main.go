// Command egress-emr is the emr service's own CONNECT proxy (EMR design
// §6.4; plan Task 13): the only container on the egress-emr network, the
// emr service's only way out. It resolves each allowed name once, checks
// every address, and dials the address it checked. It has no database, no
// NATS and no credential but K_proxy; it reads the allow-list the emr
// service writes (read-only mount) and refuses everything without a fresh
// one.
//
// Settings:
//
//	EGRESS_EMR_LISTEN          listen address (default :3128)
//	EGRESS_EMR_ALLOW_LIST      the allow-list file (required)
//	EGRESS_EMR_KEY_PATH        K_proxy (required; 64 hex characters)
//	EGRESS_EMR_INTERNAL_CIDRS  networks never reached, comma-separated (the
//	                           backend network); the proxy's own networks
//	                           are added
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/proxyconf"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/logx"
)

func main() {
	if err := run(os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "egress-emr:", err)
		os.Exit(1)
	}
}

// settings are read from the environment; errors name the variable only.
type settings struct {
	listen, allowList, keyPath string
	internal                   []netip.Prefix
}

func readSettings(getenv func(string) string) (settings, error) {
	s := settings{listen: strings.TrimSpace(getenv("EGRESS_EMR_LISTEN")),
		allowList: strings.TrimSpace(getenv("EGRESS_EMR_ALLOW_LIST")), keyPath: strings.TrimSpace(getenv("EGRESS_EMR_KEY_PATH"))}
	if s.listen == "" {
		s.listen = ":3128"
	}
	var errs []error
	if s.allowList == "" {
		errs = append(errs, errors.New("EGRESS_EMR_ALLOW_LIST is required"))
	}
	if s.keyPath == "" {
		errs = append(errs, errors.New("EGRESS_EMR_KEY_PATH is required"))
	}
	for _, part := range strings.Split(getenv("EGRESS_EMR_INTERNAL_CIDRS"), ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			errs = append(errs, errors.New("EGRESS_EMR_INTERNAL_CIDRS must be CIDR networks, comma-separated"))
			break
		}
		s.internal = append(s.internal, p.Masked())
	}
	return s, errors.Join(errs...)
}

// ownNetworks are the networks the proxy's interfaces are on (emr-proxy,
// egress-emr): never reached through it.
func ownNetworks() []netip.Prefix {
	var out []netip.Prefix
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, i := range ifs {
		as, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range as {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			addr, ok := netip.AddrFromSlice(n.IP)
			ones, _ := n.Mask.Size()
			if !ok {
				continue
			}
			out = append(out, netip.PrefixFrom(addr.Unmap(), ones).Masked())
		}
	}
	return out
}

func run(getenv func(string) string) error {
	s, err := readSettings(getenv)
	if err != nil {
		return err
	}
	key, err := proxyconf.ReadKey(s.keyPath)
	if err != nil {
		return errors.New("EGRESS_EMR_KEY_PATH: K_proxy missing or not a 32-byte hex key")
	}
	log := logx.New(os.Stdout)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	loader := &Loader{Path: s.allowList}
	loader.Reload()
	go loader.Run(ctx, proxyconf.ReloadEvery, func(ok bool) { log.Info("allow_list", "usable", ok) })

	internal := append(s.internal, ownNetworks()...)
	p := &Proxy{Key: key, List: loader.Current, Internal: internal,
		Resolver: &net.Resolver{PreferGo: true}, Dialer: &net.Dialer{Timeout: DialTimeout}, Log: log}
	srv := &http.Server{Addr: s.listen, Handler: p, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 8 << 10,
		IdleTimeout: 30 * time.Second, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("egress-emr listening", "allow_list_usable", loader.Current() != nil, "internal_networks", len(internal))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.New("egress-emr: cannot listen")
	}
	return nil
}
