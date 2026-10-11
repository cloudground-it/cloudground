//go:build !linux

// Package early makes the gate client not dumpable before anything else of
// it runs; the gate exists on Linux only, so elsewhere it does nothing.
package early

// Dumpable reports false: there is no such flag here.
func Dumpable() (bool, error) { return false, nil }
