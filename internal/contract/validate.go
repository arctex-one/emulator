package contract

import (
	_ "embed"
	"io"
	"strings"

	"github.com/arctex-one/api-protocol-go/request"
)

//go:embed openapi.json
var document string

// OpenAPI reads the published schema verified during build preparation.
func OpenAPI() io.Reader {
	return strings.NewReader(document)
}

// Request validates the request using the emulator's public contract revision.
func Request(raw []byte, schema string) ([]byte, error) {
	return request.Canonicalize([]byte(document), schema, raw)
}
