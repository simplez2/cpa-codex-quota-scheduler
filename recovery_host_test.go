package main

import (
	"runtime"
	"strings"
	"testing"
)

func TestRecoveryHostMarkerStableWithinProcess(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		t.Skip("no supported native release for this OS")
	}
	first := currentRecoveryHostMarker()
	if first == "" || !strings.HasPrefix(first, runtime.GOOS+":") {
		t.Fatalf("supported native platform has no process identity: %q", first)
	}
	for i := 0; i < 3; i++ {
		if next := currentRecoveryHostMarker(); next != first {
			t.Fatalf("process identity changed without a restart: %q != %q", next, first)
		}
	}
}
