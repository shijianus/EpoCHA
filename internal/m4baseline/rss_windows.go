//go:build windows

package m4baseline

import (
	"syscall"
	"unsafe"
)

type processMemoryCounters struct {
	Cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

var procGetProcessMemoryInfo = syscall.NewLazyDLL("psapi.dll").NewProc("GetProcessMemoryInfo")

// rssBytes 返回当前进程的工作集（Windows 上的 RSS 等价量）。
// 它是整个进程的占用，包含 Go runtime、生成器与 harness，不是"只算 solver"。
func rssBytes() uint64 {
	var c processMemoryCounters
	c.Cb = uint32(unsafe.Sizeof(c))
	proc, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0
	}
	r, _, _ := procGetProcessMemoryInfo.Call(uintptr(proc), uintptr(unsafe.Pointer(&c)), uintptr(c.Cb))
	if r == 0 {
		return 0
	}
	return uint64(c.WorkingSetSize)
}
