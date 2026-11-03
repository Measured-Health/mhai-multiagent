package intel

import (
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
)

// Every fact intelligence may hand core has a schema in core's audit
// registry too, so a fact that passes intel's check is never refused by
// audit.Write (which would stall the drainer).
func TestFactActionsHaveAuditSchemas(t *testing.T) {
	for a := range jobFactSchemas {
		if !audit.Registered(a) {
			t.Errorf("job fact %s has no audit schema", a)
		}
	}
	for a := range replyFactSchemas {
		if !audit.Registered(a) {
			t.Errorf("reply fact %s has no audit schema", a)
		}
	}
}
