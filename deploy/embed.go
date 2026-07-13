package deployfiles

import _ "embed"

//go:embed compose.yaml
var Compose []byte

//go:embed Caddyfile
var Caddyfile []byte
