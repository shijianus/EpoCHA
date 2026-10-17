package main

import "testing"

func TestRunVersionFlag(t *testing.T) {
	// 真实断言在阶段 1 补：--version 输出格式与 ldflags 注入值一致。
	t.Skip("待阶段 1：main 的 flag 解析与启动顺序（config→store→http）落地后补断言")
}
