package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStopWaitsForAdmittedWorkerAndRejectsNewWorkers(t *testing.T) {
	state := schedulerRuntimeState{cfg: defaultPluginConfig()}
	state.cfg.StatePath = ""
	if !state.admitBackgroundWorker() {
		t.Fatal("initial worker was not admitted")
	}
	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		defer state.wg.Done()
		close(started)
		<-release
	}()
	<-started

	stopped := make(chan struct{})
	go func() {
		state.stop()
		close(stopped)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		state.mu.RLock()
		stopping := state.stopping
		state.mu.RUnlock()
		if stopping {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stop did not close worker admission")
		}
		time.Sleep(time.Millisecond)
	}
	if state.admitBackgroundWorker() {
		state.wg.Done()
		t.Fatal("worker admitted after shutdown began")
	}
	select {
	case <-stopped:
		t.Fatal("stop returned before the admitted worker completed")
	default:
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stop did not return after the admitted worker completed")
	}
}

func TestConcurrentReconfigureAndStopSerializesWorkerLifecycle(t *testing.T) {
	schedulerRuntime.stop()
	t.Cleanup(func() {
		configureSchedulerRuntime([]byte("{\"enabled\":false,\"state_path\":\"\"}"))
	})

	passwordPath := filepath.Join(t.TempDir(), "keeper-password")
	if err := os.WriteFile(passwordPath, []byte("test-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(t.TempDir(), "scheduler-state.json")
	raw := []byte(fmt.Sprintf(
		"{\"enabled\":true,\"keeper_url\":\"http://127.0.0.1:1\",\"keeper_password_file\":%q,\"refresh_interval\":\"1s\",\"state_path\":%q,\"warmup_enabled\":false}",
		filepath.ToSlash(passwordPath), filepath.ToSlash(statePath),
	))

	var wg sync.WaitGroup
	for iteration := 0; iteration < 16; iteration++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			configureSchedulerRuntime(raw)
		}()
		go func() {
			defer wg.Done()
			schedulerRuntime.stop()
		}()
	}
	wg.Wait()
	// The last concurrent operation may have been a configure, so make one
	// final ordered stop and verify admission remains closed.
	schedulerRuntime.stop()
	if schedulerRuntime.admitBackgroundWorker() {
		schedulerRuntime.wg.Done()
		t.Fatal("worker admitted after the final serialized stop")
	}
}
