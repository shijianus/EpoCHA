package pool

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/internal/store"
)

func TestWorkerPrewarmsRefreshDifficulties(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Prewarm = true
		c.LowWatermark = 2
		c.HighWatermark = 4
		c.BatchPerTick = 4
		c.ImageLowWatermark = 2
		c.ImageHighWatermark = 4
	}, nil)
	bank, err := generator.NewBankShot(bytes.Repeat([]byte{0x51}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.reg.Register(bank); err != nil {
		t.Fatal(err)
	}
	site, _, err := h.db.CreateSite(context.Background(), "prewarm", "sk-prewarm", store.SitePolicy{
		PowBits: 12, VerifyMode: store.ModePowQuestion,
		GeneratorID: generator.BankShotID, GeneratorPool: []string{generator.BankShotID}, Difficulty: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := h.w.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
	}
	count := func(d int) int {
		n, err := h.db.ReadyCount(context.Background(), store.PoolSpec{
			GeneratorID: generator.BankShotID, Version: "v1", Difficulty: d,
		}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count(3) < 2 {
		t.Fatalf("基础难度 d3 ready = %d，期望至少 2", count(3))
	}
	if count(4) < 1 || count(5) < 1 {
		t.Fatalf("预热难度缺货: d4=%d d5=%d", count(4), count(5))
	}
	if site.Policy.Difficulty != 3 {
		t.Fatal("预热不应改写站点基础难度")
	}
}

func TestAccessibleChoiceIsNotPrewarmed(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Prewarm = true
		c.LowWatermark = 2
		c.HighWatermark = 4
		c.BatchPerTick = 4
	}, nil)
	acc, err := generator.NewAccessibleChoice(bytes.Repeat([]byte{0x52}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.reg.Register(acc); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.db.CreateSite(context.Background(), "a11y", "sk-a11y-prewarm", store.SitePolicy{
		PowBits: 12, VerifyMode: store.ModePowQuestion,
		GeneratorID: generator.AccessibleChoiceID, GeneratorPool: []string{generator.AccessibleChoiceID}, Difficulty: 3,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := h.w.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []int{4, 5} {
		n, err := h.db.ReadyCount(context.Background(), store.PoolSpec{
			GeneratorID: generator.AccessibleChoiceID, Version: "v1", Difficulty: d,
		}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("无障碍固定难度不应预热 d%d，got %d", d, n)
		}
	}
}
