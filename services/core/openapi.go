// Package core embeds the committed API contract so the service can serve
// the very bytes that internal/auth/contract_test.go checks.
package core

import _ "embed"

// OpenAPI is services/core/openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPI []byte
