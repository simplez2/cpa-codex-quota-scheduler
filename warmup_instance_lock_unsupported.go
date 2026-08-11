//go:build !windows && !unix

package main

import (
	"fmt"
	"os"
	"runtime"
)

func tryExclusiveFileLock(_ *os.File) (bool, error) {
	return false, fmt.Errorf("cross-instance warmup locking is unsupported on %s", runtime.GOOS)
}

func unlockExclusiveFile(_ *os.File) error {
	return nil
}
