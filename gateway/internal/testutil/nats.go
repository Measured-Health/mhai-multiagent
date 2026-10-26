package testutil

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"time"

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
// deploy/nats/nats.conf. The URLs carry each service's test credentials.
type NATS struct {
	GatewayURL string
	WorkerURL  string
}

// NewNATS starts the container. The config is copied in rather than
// bind-mounted, so it works when the tests themselves run in a container.
func NewNATS(ctx context.Context) (n *NATS, stop func(), err error) {
	_, file, _, _ := runtime.Caller(0)
	conf := filepath.Join(filepath.Dir(file), "..", "..", "deploy", "nats", "nats.conf")
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
		GatewayURL: fmt.Sprintf("nats://gateway:%s@%s", natsGatewayPassword, addr),
		WorkerURL:  fmt.Sprintf("nats://worker:%s@%s", natsWorkerPassword, addr),
	}, stop, nil
}
