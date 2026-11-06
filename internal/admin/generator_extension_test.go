package admin

import (
	"strings"
	"testing"

	"github.com/shijianus/EpoCHA/internal/generator"
)

// TestExtensionGeneratorsStayExperimental 钉住 ADR-024 的边界：
// 新题型在得到 M4/人工可读性证据之前不得被标成生产可用。
func TestExtensionGeneratorsStayExperimental(t *testing.T) {
	for _, id := range []string{
		generator.TextImageV3ID, generator.ShapeGridID, generator.ShapeCountID,
		generator.ParallaxID, generator.BankShotID, generator.AccessibleChoiceID,
		generator.EnergyWindowID, generator.PipeConnectID,
		generator.IntersectionPointID,
	} {
		exp, ready := generatorLifecycle(id)
		if !exp || ready {
			t.Fatalf("%s 的生命周期 = (%v,%v)，必须 experimental=true / production_ready=false", id, exp, ready)
		}
		note := noteForGenerator(id)
		if note == "" {
			t.Fatalf("%s 没有管理面说明", id)
		}
		if !strings.Contains(note, "production_ready") {
			t.Fatalf("%s 的说明没有写 production_ready 状态: %q", id, note)
		}
	}
	if note := noteForGenerator(generator.ShapeGridID); !strings.Contains(note, "M4 未测") {
		t.Fatalf("shape-grid 说明没有写 M4 未测: %q", note)
	}
	if note := noteForGenerator(generator.ShapeCountID); !strings.Contains(note, "M4 未测") {
		t.Fatalf("shape-count 说明没有写 M4 未测: %q", note)
	}
	if note := noteForGenerator(generator.TextImageV3ID); !strings.Contains(note, "NOT_RUN") {
		t.Fatalf("text-image-v3 说明没有写 NOT_RUN: %q", note)
	}
	if note := noteForGenerator(generator.ParallaxID); !strings.Contains(note, "M4") {
		t.Fatalf("parallax 说明没有写 M4 状态: %q", note)
	}
	if note := noteForGenerator(generator.BankShotID); !strings.Contains(note, "M4") {
		t.Fatalf("bank-shot 说明没有写 M4 状态: %q", note)
	}
}
