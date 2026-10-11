// Package migrations bundles the .sql files into the Go binary so the
// migrate command works from any directory.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
