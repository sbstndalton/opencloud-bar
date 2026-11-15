//go:build notray

package main

import "context"

// runTray is a stand-in for builds without the systray library (tests on macOS, which
// can't build it without cgo). Release builds never use the notray tag.
func runTray(e *Engine, ctx context.Context, quit func()) { <-ctx.Done() }
