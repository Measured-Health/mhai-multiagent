//go:build integration

package bus_test

import (
	"context"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// Connect wires the link's handlers: stopping the real server makes Up()
// false (so /readyz answers not_ready) without anyone calling the handlers.
func TestConnectWiresLinkHandlers(t *testing.T) {
	ctx := context.Background()
	n, stop, err := testutil.NewNATS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			stop()
		}
	}()
	link := bus.NewLink(nil)
	nc, _, err := bus.Connect(ctx, n.GatewayURL, "", bus.GatewayInboxPrefix, bus.TLS{}, link.Options()...)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if !link.Up() {
		t.Fatal("link down while connected")
	}
	stop()
	stopped = true
	deadline := time.Now().Add(20 * time.Second)
	for link.Up() && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if link.Up() {
		t.Fatal("link still up after the server stopped")
	}
}
