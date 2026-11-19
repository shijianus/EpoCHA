//go:build !linux && !windows

package m4baseline

import "errors"

func processCPUSeconds() (float64, float64, error) {
	return 0, 0, errors.New("OS process CPU collector unavailable")
}
