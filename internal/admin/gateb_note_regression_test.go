package admin

import (
	"strings"
	"testing"

	"github.com/shijianus/EpoCHA/internal/generator"
)

// TestGateBNotesKeepMeasuredHistory 钉住 ADR-034 §8 的事实纪律：
// Gate B 七型的备注必须保留 2026-11-12/11-14 两次实测的 FAIL（旧 FAIL 不能抹掉），
// 必须写明"当前源码未复测"（数字不自动继承），且不得把已实测写成 NOT_RUN。
func TestGateBNotesKeepMeasuredHistory(t *testing.T) {
	for _, id := range []string{
		generator.TileRotationID, generator.SymbolPathID, generator.CrowdFindID,
		generator.VisualRankID, generator.ProgressiveTargetID, generator.ShellTrackID,
		generator.SilhouetteAssemblyID,
	} {
		note := noteForGenerator(id)
		if !strings.Contains(note, "2026-11-14 复核均为 FAIL") {
			t.Errorf("%s 备注缺实测 FAIL 历史: %q", id, note)
		}
		if !strings.Contains(note, "当前源码未复测") {
			t.Errorf("%s 备注未区分当前候选与旧测量: %q", id, note)
		}
		if strings.Contains(note, "M4/人工可读性/辅助技术 NOT_RUN") {
			t.Errorf("%s 已实测却仍写 NOT_RUN: %q", id, note)
		}
		if !strings.Contains(note, "production_ready 保持 false") {
			t.Errorf("%s 备注必须保持 production_ready false: %q", id, note)
		}
	}
	// 非 Gate B 题型（未实测）保持"未测"表述，不得冒充实测。
	for _, id := range []string{generator.AccessibleChoiceID, generator.PipeConnectID, generator.ShapeGridID} {
		note := noteForGenerator(id)
		if strings.Contains(note, "复核均为 FAIL") {
			t.Errorf("%s 未实测不得写 FAIL 历史: %q", id, note)
		}
		if !strings.Contains(note, "未测") && !strings.Contains(note, "NOT_RUN") {
			t.Errorf("%s 未实测应保持未测表述: %q", id, note)
		}
	}
}
