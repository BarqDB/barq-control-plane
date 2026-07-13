package contracts

import _ "embed"

//go:embed public-openapi.yaml
var publicOpenAPI []byte

func PublicOpenAPI() []byte { return append([]byte(nil), publicOpenAPI...) }
