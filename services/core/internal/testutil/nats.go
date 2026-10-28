package testutil

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/nats-io/nkeys"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// NATSImage is the server every integration test runs against.
const NATSImage = "nats:2.10-alpine"

// Throwaway passwords for the test server only.
const (
	natsGatewayPassword = "test-only-gateway-pw"
	natsWorkerPassword  = "test-only-worker-pw"
)

// NATS is a throwaway NATS server with JetStream and the real
// services/nats/nats.conf. The URLs carry each service's test credentials.
type NATS struct {
	Addr       string // host:port, no credentials
	GatewayURL string
	WorkerURL  string
}

// NewNATS starts the container. The config is copied in rather than
// bind-mounted, so it works when the tests themselves run in a container.
func NewNATS(ctx context.Context) (n *NATS, stop func(), err error) {
	_, file, _, _ := runtime.Caller(0)
	conf := filepath.Join(filepath.Dir(file), "..", "..", "..", "nats", "nats.conf")
	return startNATS(ctx, conf)
}

// NATSNkey is a throwaway server with one NKey user allowed to publish
// inference.v1.> only, for testing the production authentication path.
type NATSNkey struct {
	URL      string // no credentials
	SeedPath string // the user's seed file
}

// NewNATSNkey starts a server whose only user authenticates by NKey.
func NewNATSNkey(ctx context.Context, dir string) (n *NATSNkey, stop func(), err error) {
	kp, err := nkeys.CreateUser()
	if err != nil {
		return nil, nil, err
	}
	pub, err := kp.PublicKey()
	if err != nil {
		return nil, nil, err
	}
	seed, err := kp.Seed()
	if err != nil {
		return nil, nil, err
	}
	conf := filepath.Join(dir, "nkey.conf")
	body := fmt.Sprintf(`port: 4222
jetstream { store_dir: /data }
authorization { users = [ { nkey: %s, permissions: { publish: { allow: ["inference.v1.>"] }, subscribe: { allow: ["_INBOX_gateway.>"] } } } ] }
`, pub)
	if err := os.WriteFile(conf, []byte(body), 0o644); err != nil {
		return nil, nil, err
	}
	seedPath := filepath.Join(dir, "user.seed")
	if err := os.WriteFile(seedPath, seed, 0o600); err != nil {
		return nil, nil, err
	}
	srv, stop, err := startNATS(ctx, conf)
	if err != nil {
		return nil, nil, err
	}
	return &NATSNkey{URL: "nats://" + srv.Addr, SeedPath: seedPath}, stop, nil
}

func startNATS(ctx context.Context, conf string) (n *NATS, stop func(), err error) {
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		Started: true,
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        NATSImage,
			ExposedPorts: []string{"4222/tcp"},
			Cmd:          []string{"-c", "/etc/nats/nats.conf"},
			Env: map[string]string{
				"NATS_GATEWAY_PASSWORD": natsGatewayPassword,
				"NATS_WORKER_PASSWORD":  natsWorkerPassword,
			},
			Files: []testcontainers.ContainerFile{{
				HostFilePath:      conf,
				ContainerFilePath: "/etc/nats/nats.conf",
				FileMode:          0o644,
			}},
			WaitingFor: wait.ForLog("Server is ready").WithStartupTimeout(60 * time.Second),
		},
	})
	if err != nil {
		if c != nil {
			_ = testcontainers.TerminateContainer(c)
		}
		return nil, nil, err
	}
	stop = func() { _ = testcontainers.TerminateContainer(c) }
	host, err := c.Host(ctx)
	if err != nil {
		stop()
		return nil, nil, err
	}
	port, err := c.MappedPort(ctx, "4222/tcp")
	if err != nil {
		stop()
		return nil, nil, err
	}
	addr := fmt.Sprintf("%s:%s", host, port.Port())
	return &NATS{
		Addr:       addr,
		GatewayURL: fmt.Sprintf("nats://gateway:%s@%s", natsGatewayPassword, addr),
		WorkerURL:  fmt.Sprintf("nats://worker:%s@%s", natsWorkerPassword, addr),
	}, stop, nil
}
