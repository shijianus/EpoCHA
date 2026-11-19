//go:build linux

package m4baseline

import (
	"os"
	"strconv"
	"strings"
)

// rssBytes 从 /proc/self/statm 读 resident pages。它是整个进程的 RSS，不是 solver 增量。
func rssBytes() uint64 {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * uint64(os.Getpagesize())
}
