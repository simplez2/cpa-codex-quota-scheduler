//go:build linux

package main

import (
	"fmt"
	"os"
	"strings"
)

// The process start tick and kernel boot identifier stay stable across plugin
// reloads, while distinguishing PID reuse and container-local PID 1 restarts.
func currentRecoveryHostMarker() string {
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return ""
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return ""
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) <= 19 || strings.TrimSpace(string(boot)) == "" {
		return ""
	}
	return fmt.Sprintf("linux:%s:%d:%s", strings.TrimSpace(string(boot)), os.Getpid(), fields[19])
}
