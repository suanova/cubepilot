// Package ui embeds the CubePilot Portal so the assistant service serves it
// from the binary (no runtime file dependency).
package ui

import _ "embed"

// IndexHTML is the wired Portal page (chat + tasks + audit + agent config).
//
//go:embed index.html
var IndexHTML string
