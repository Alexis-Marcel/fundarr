// Package docs holds the OpenAPI description of the read API, served by
// Fundarr at /api/v1/openapi.yaml.
package docs

import _ "embed"

//go:embed openapi.yaml
var OpenAPI []byte
