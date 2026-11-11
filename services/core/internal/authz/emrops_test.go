package authz

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

// Plan departure 2 (P2): the emr service's control operations live in
// gokit/emrops, never in this table. This table is published to the
// Python workers as operations.json, and its rows allow clinical roles
// only; an emr row there would let a worker accept a platform role.
func TestNoEMROpsInOperationsJSON(t *testing.T) {
	for _, o := range Operations() {
		if o.Service == "emr" || strings.HasPrefix(o.Name, "emr.") {
			t.Errorf("operations table has %s", o.Name)
		}
	}
	b, err := OperationsJSON(Operations())
	if err != nil {
		t.Fatal(err)
	}
	committed, err := os.ReadFile(filepath.Join("..", "..", "..", "intelligence", "intelligence", "boundary", "operations.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range [][]byte{b, committed} {
		if strings.Contains(string(doc), `"emr.`) || strings.Contains(string(doc), `"service": "emr"`) {
			t.Errorf("operations.json names an emr operation")
		}
	}
	// And the table refuses one: platform_admin isn't a clinical role.
	if err := validateOperation(Operation{Name: "emr.test", Service: "emr", Op: "test", Kind: reqtoken.KindReq,
		AnyRole: []string{"platform_admin"}, NeedsHospital: true, RequestSchema: "mhai.emr.v1.TestRequest",
		ResponseSchema: "mhai.emr.v1.TestReply"}); err == nil {
		t.Error("validateOperation accepted an emr row with platform_admin")
	}
}
