//go:build !linux && !windows

package main

// Without a stable process-start identity, preserve a persisted in-flight lease.
func currentRecoveryHostMarker() string { return "" }
