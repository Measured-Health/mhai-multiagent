package main

import (
	"context"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/config"
)

// K4: with CORE_EMR_ENABLED off (the default) serve creates no EMR stream:
// ensureEMR doesn't touch JetStream at all (a nil one here). On, it does,
// and a failure stops serve.
func TestEMRStreamNotCreatedWhenDisabled(t *testing.T) {
	if err := ensureEMR(context.Background(), config.Config{}, nil); err != nil {
		t.Fatalf("off: %v", err)
	}
	if err := ensureEMR(context.Background(), config.Config{EMREnabled: true, NATSJSReplicas: 1}, nil); err == nil {
		t.Fatal("on: no error without JetStream")
	}
}
