package store

import (
	"context"
	"errors"
	"strconv"
	"testing"
)

func TestSiteGeneratorPoolPersistenceAndValidation(t *testing.T) {
	d := newDB(t)
	p := DefaultPolicy()
	p.PowBits = 12
	p.VerifyMode = ModePowQuestion
	p.GeneratorID = "bank-shot-v1"
	p.GeneratorPool = []string{"bank-shot-v1", "parallax-code-v1"}
	p.Difficulty = 2
	site, _, err := d.CreateSite(context.Background(), "pooled", "sk-pooled", p)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if got := site.Policy.GeneratorIDs(); len(got) != 2 || got[0] != "bank-shot-v1" || got[1] != "parallax-code-v1" {
		t.Fatalf("GeneratorIDs = %v", got)
	}
	read, err := d.SiteByKey(context.Background(), "sk-pooled")
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Policy.GeneratorPool) != 2 {
		t.Fatalf("读回 pool = %v", read.Policy.GeneratorPool)
	}
	baseBuckets, err := d.QuestionBuckets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	prewarm := map[string]bool{}
	for _, b := range baseBuckets {
		if b.Prewarm {
			prewarm[b.GeneratorID+"|"+strconv.Itoa(b.Difficulty)] = true
		}
	}
	for _, id := range []string{"bank-shot-v1", "parallax-code-v1"} {
		for d := 3; d <= 5; d++ {
			if !prewarm[id+"|"+strconv.Itoa(d)] {
				t.Fatalf("缺少预热桶 %s|d%d: %+v", id, d, baseBuckets)
			}
		}
	}
	next := []string{"shape-grid-v1"}
	updated, err := d.UpdateSite(context.Background(), site.ID, SiteUpdate{GeneratorPool: &next})
	if err != nil {
		t.Fatalf("UpdateSite pool: %v", err)
	}
	if len(updated.Policy.GeneratorPool) != 1 || updated.Policy.GeneratorPool[0] != "shape-grid-v1" {
		t.Fatalf("更新后 pool = %v", updated.Policy.GeneratorPool)
	}
	buckets, err := d.QuestionBuckets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, b := range buckets {
		seen[b.GeneratorID] = true
	}
	if !seen["shape-grid-v1"] {
		t.Fatalf("QuestionBuckets 未展开更新后的池: %+v", buckets)
	}
	// 空池回落单 generator_id。
	empty := []string{}
	updated, err = d.UpdateSite(context.Background(), site.ID, SiteUpdate{GeneratorPool: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Policy.GeneratorIDs(); len(got) != 1 || got[0] != "bank-shot-v1" {
		t.Fatalf("空池回落 = %v", got)
	}

	for _, bad := range [][]string{
		{"bank-shot-v1", "bank-shot-v1"},
		{"", "bank-shot-v1"},
		{"a", "b", "c", "d", "e", "f", "g", "h", "i"},
	} {
		pp := p
		pp.GeneratorPool = bad
		if _, _, err := d.CreateSite(context.Background(), "bad", "sk-bad-"+randSuffix(), pp); !errors.Is(err, ErrBadConfig) {
			t.Fatalf("非法 pool %v 应 ErrBadConfig，got %v", bad, err)
		}
	}
}
