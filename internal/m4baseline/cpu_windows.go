//go:build windows

package m4baseline

import "syscall"

func filetimeDurationSeconds(t syscall.Filetime) float64 {
	return float64(uint64(t.HighDateTime)<<32|uint64(t.LowDateTime)) / 1e7
}

func processCPUSeconds() (user, system float64, err error) {
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, 0, err
	}
	var created, exited, kernel, used syscall.Filetime
	if err := syscall.GetProcessTimes(h, &created, &exited, &kernel, &used); err != nil {
		return 0, 0, err
	}
	return filetimeDurationSeconds(used), filetimeDurationSeconds(kernel), nil
}
