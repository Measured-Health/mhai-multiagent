package connector

import (
	"slices"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrtypes"
)

// Registry holds the connectors this process runs.
type Registry struct {
	byID map[string]Connector
}

// NewRegistry registers each of cs whose type is built for this
// environment (emrtypes.Built): a development-only type (the stub) only
// when development is true (APP_ENV=development). Others are dropped.
func NewRegistry(development bool, cs ...Connector) *Registry {
	r := &Registry{byID: map[string]Connector{}}
	built := emrtypes.Built(development)
	for _, c := range cs {
		if id := c.Type().ID; slices.Contains(built, id) {
			r.byID[id] = c
		}
	}
	return r
}

// Get returns the connector for type id.
func (r *Registry) Get(id string) (Connector, bool) {
	c, ok := r.byID[id]
	return c, ok
}

// Offered are the registered types among enabled, in the table's order.
func (r *Registry) Offered(enabled []string) []TypeInfo {
	var out []TypeInfo
	for _, t := range emrtypes.Types {
		if _, ok := r.byID[t.ID]; ok && slices.Contains(enabled, t.ID) {
			out = append(out, t)
		}
	}
	return out
}
