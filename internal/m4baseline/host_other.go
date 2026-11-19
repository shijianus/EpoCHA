//go:build !windows && !linux

package m4baseline

func hostCPUModel() string { return "" }
func hostTotalRAM() uint64 { return 0 }
func rssBytes() uint64     { return 0 }
