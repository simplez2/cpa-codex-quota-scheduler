//go:build darwin && cgo

package main

/*
#cgo darwin LDFLAGS: -lproc
#include <libproc.h>
#include <stdint.h>
#include <sys/proc_info.h>
#include <unistd.h>

static int recovery_process_start(uint64_t* seconds, uint64_t* microseconds) {
	struct proc_bsdinfo info;
	int size = proc_pidinfo(getpid(), PROC_PIDTBSDINFO, 0, &info, sizeof(info));
	if (size != sizeof(info)) {
		return 0;
	}
	*seconds = info.pbi_start_tvsec;
	*microseconds = info.pbi_start_tvusec;
	return 1;
}
*/
import "C"

import (
	"fmt"
	"os"
)

// Kernel process birth time stays stable across independently loaded plugin
// runtimes and distinguishes a restarted CPA, including reuse of its PID.
func currentRecoveryHostMarker() string {
	var seconds, microseconds C.uint64_t
	if C.recovery_process_start(&seconds, &microseconds) == 0 {
		return ""
	}
	return fmt.Sprintf("darwin:%d:%d:%d", os.Getpid(), uint64(seconds), uint64(microseconds))
}
