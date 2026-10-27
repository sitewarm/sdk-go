package sitewarm

// Regenerates models/models.gen.go (types, oapi-codegen) and ops_gen.go (operationId table) from the
// vendored spec. CI runs `go generate ./...` and fails on drift. Webhook callbacks are not part of the SDK
// and are removed from a temporary copy of the spec first.

//go:generate go run -C tools ./genops prep ../openapi/openapi.yaml ../.prep-openapi.yaml
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.1 -config oapi-codegen.yaml .prep-openapi.yaml
//go:generate go run -C tools ./genops ../openapi/openapi.yaml ../ops_gen.go
