package config

import "testing"

func TestMissingMasterKeyRefusesToStart(t *testing.T) {
	t.Skip("待阶段 1（DESIGN §6）：master key 或 pepper 缺失 → 返回错误，不得用默认值静默启动")
}

func TestPowBitsRangeValidated(t *testing.T) {
	t.Skip("待阶段 2：bits 限定 12..24，越界拒绝并给出可诊断错误")
}
