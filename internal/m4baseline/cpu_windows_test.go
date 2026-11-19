//go:build windows

package m4baseline

import (
	"syscall"
	"testing"
)

func TestFiletimeCPUIsDurationNotEpoch(t *testing.T) {
	if got := filetimeDurationSeconds(syscall.Filetime{LowDateTime: 10_000_000}); got != 1 {
		t.Fatalf("CPU duration=%v want 1s", got)
	}
	if got := filetimeDurationSeconds(syscall.Filetime{HighDateTime: 1}); got != float64(uint64(1)<<32)/1e7 {
		t.Fatalf("high word lost: %v", got)
	}
	user, system, err := processCPUSeconds()
	if err != nil || user < 0 || system < 0 {
		t.Fatalf("OS CPU collection: %v %v %v", user, system, err)
	}
}
