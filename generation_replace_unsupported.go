//go:build !windows && !unix

package main

import "os"

func replaceGenerationJournal(tempPath, targetPath string) error {
	return os.Rename(tempPath, targetPath)
}
