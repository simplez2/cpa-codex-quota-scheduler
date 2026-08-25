//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

const (
	moveFileReplaceExisting = 0x00000001
	moveFileWriteThrough    = 0x00000008
)

var moveFileExProc = kernel32DLL.NewProc("MoveFileExW")

func replaceGenerationJournal(tempPath, targetPath string) error {
	tempUTF16, err := syscall.UTF16PtrFromString(tempPath)
	if err != nil {
		return err
	}
	targetUTF16, err := syscall.UTF16PtrFromString(targetPath)
	if err != nil {
		return err
	}
	result, _, callErr := moveFileExProc.Call(
		uintptr(unsafe.Pointer(tempUTF16)),
		uintptr(unsafe.Pointer(targetUTF16)),
		uintptr(moveFileReplaceExisting|moveFileWriteThrough),
	)
	if result != 0 {
		return nil
	}
	if callErr == syscall.Errno(0) {
		return os.ErrInvalid
	}
	return callErr
}
