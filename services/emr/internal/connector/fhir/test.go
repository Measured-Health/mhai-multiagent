package fhir

// The test (design §5.1, §5.2): sign in, read the capability statement
// (FHIR 4.0.x; every resource we read, with _lastUpdated and _sort
// declared), and ask for a count of observations (a count, no record).
// Read-only; a closed code.

import (
	"context"
	"slices"
	"strings"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
)

// Required are the resource types the connector reads.
var Required = []string{"Patient", "Observation", "MedicationRequest", "MedicationStatement", "Encounter", "Location"}

type searchParam struct {
	Name string `json:"name"`
}

type capabilityStatement struct {
	ResourceType string `json:"resourceType"`
	FHIRVersion  string `json:"fhirVersion"`
	Rest         []struct {
		Mode        string        `json:"mode"`
		SearchParam []searchParam `json:"searchParam"`
		Resource    []struct {
			Type        string        `json:"type"`
			SearchParam []searchParam `json:"searchParam"`
		} `json:"resource"`
	} `json:"rest"`
}

func names(ps []searchParam) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

// supported reports whether the statement is R4 (4.0.x) and a server
// declares every required type with _lastUpdated and _sort.
func (cs capabilityStatement) supported() bool {
	if cs.ResourceType != "CapabilityStatement" || !strings.HasPrefix(cs.FHIRVersion, "4.0.") {
		return false
	}
	for _, rest := range cs.Rest {
		if rest.Mode != "server" {
			continue
		}
		common := names(rest.SearchParam)
		ok := true
		for _, t := range Required {
			found := false
			for _, r := range rest.Resource {
				if r.Type != t {
					continue
				}
				ps := append(names(r.SearchParam), common...)
				found = slices.Contains(ps, "_lastUpdated") && slices.Contains(ps, "_sort")
				break
			}
			if !found {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// Test is the read-only probe.
func (c *FHIR) Test(ctx context.Context, cfg connector.Config, creds connector.Creds) connector.TestResult {
	if err := c.test(ctx, cfg, creds); err != nil {
		return connector.Result(connector.CodeOf(err))
	}
	return connector.Result("ok")
}

func (c *FHIR) test(ctx context.Context, cfg connector.Config, creds connector.Creds) error {
	client, err := c.client(cfg)
	if err != nil {
		return err
	}
	base := strings.TrimSuffix(cfg.BaseURL, "/")
	if !onOrigin(base, cfg.Origin) {
		return signInRefused()
	}
	// A fresh sign-in, never the cache: the test is of the credentials.
	tok, _, err := Token(ctx, client, cfg, creds, c.now())
	if err != nil {
		return err
	}
	var cs capabilityStatement
	if err := c.getJSON(ctx, client, base+"/metadata", tok, &cs); err != nil {
		return err
	}
	if !cs.supported() {
		return connector.Fail("unreadable_data", 0)
	}
	var b bundle
	if err := c.getJSON(ctx, client, base+"/Observation?_summary=count", tok, &b); err != nil {
		return err
	}
	if b.ResourceType != "Bundle" {
		return connector.Fail("unreadable_data", 0)
	}
	return nil
}
