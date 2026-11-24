package templates

import "embed"

//go:embed lighthouse.yml.tmpl node.yml.tmpl
var FS embed.FS
