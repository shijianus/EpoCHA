//go:build linux

package m4baseline

import "syscall"

func processCPUSeconds() (user, system float64, err error) {
	var r syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &r); err != nil {
		return 0, 0, err
	}
	return float64(r.Utime.Sec) + float64(r.Utime.Usec)/1e6,
		float64(r.Stime.Sec) + float64(r.Stime.Usec)/1e6, nil
}
