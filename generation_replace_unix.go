//go:build unix

package main

import (
	"errors"
	"os"
	"path/filepath"
)

func replaceGenerationJournal(tempPath, targetPath string) error {
	if err := os.Rename(tempPath, targetPath); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(targetPath))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
