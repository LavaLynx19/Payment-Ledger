// Package migrations embeds the goose SQL migrations into every binary.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
