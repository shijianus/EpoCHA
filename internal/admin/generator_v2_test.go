package admin

import (
	"strings"
	"testing"

	"github.com/shijianus/EpoCHA/internal/generator"
)

func TestTextImageV2LifecycleIsConservative(t *testing.T) {
	exp, ready := generatorLifecycle(generator.TextImageV2ID)
	if !exp || ready {
		t.Fatalf("v2 必须保持 experimental=true / production_ready=false，实得 %v/%v", exp, ready)
	}
	note := noteForGenerator(generator.TextImageV2ID)
	for _, want := range []string{"v2", "OCR", "production_ready"} {
		if !strings.Contains(note, want) {
			t.Fatalf("v2 note 缺少 %q：%q", want, note)
		}
	}
}
