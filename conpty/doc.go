// Package conpty is the vendored Windows ConPTY backend (see conpty.go, which
// carries the //go:build windows constraint). This unconstrained doc file keeps
// the package buildable on non-windows platforms (where conpty.go is excluded),
// so `go build ./...` on unix does not fail with "build constraints exclude all
// Go files". It intentionally declares no symbols.
package conpty
