//go:build !linux && !windows && (!darwin || !cgo)

package main

// Without a stable process-start identity, preserve a persisted in-flight lease.
func currentRecoveryHostMarker() string { return "" }
