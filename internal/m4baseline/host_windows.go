//go:build windows

package m4baseline

import (
	"syscall"
	"unsafe"
)

func hostCPUModel() string {
	const path = `HARDWARE\DESCRIPTION\System\CentralProcessor\0`
	var k syscall.Handle
	err := syscall.RegOpenKeyEx(syscall.HKEY_LOCAL_MACHINE,
		syscall.StringToUTF16Ptr(path), 0, syscall.KEY_READ, &k)
	if err != nil {
		return ""
	}
	defer syscall.RegCloseKey(k)
	var buf [256]uint16
	var typ uint32
	n := uint32(len(buf) * 2)
	if err := syscall.RegQueryValueEx(k, syscall.StringToUTF16Ptr("ProcessorNameString"),
		nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &n); err != nil {
		return ""
	}
	return syscall.UTF16ToString(buf[:])
}

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

var procGlobalMemoryStatusEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")

func hostTotalRAM() uint64 {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return 0
	}
	return m.TotalPhys
}
