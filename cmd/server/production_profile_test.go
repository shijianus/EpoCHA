package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/config"
	"github.com/shijianus/EpoCHA/internal/generator"
)

func TestP0ProductionProfileRejectsRealCatalog(t *testing.T) {
	known, err := knownGenerators(bytes.Repeat([]byte{7}, 32))
	if err != nil || len(known) != 19 {
		t.Fatalf("real catalog: count=%d error=%v", len(known), err)
	}
	for _, whitelist := range []string{"", "arithmetic-baseline-v1", "text-image-v1"} {
		t.Run("whitelist="+whitelist, func(t *testing.T) {
			env := map[string]string{
				"EPOCHA_MASTER_KEY":         base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)),
				"EPOCHA_PEPPER":             base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)),
				"EPOCHA_DB":                 filepath.Join(t.TempDir(), "production.db"),
				"EPOCHA_PRODUCTION_PROFILE": "true", "EPOCHA_GENERATORS": whitelist,
			}
			cfg, err := config.Load(nil, func(k string) string { return env[k] })
			if err != nil {
				t.Fatal(err)
			}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			err = serve(ctx, cfg, ln, nil, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "production") {
				t.Fatalf("experimental real catalog did not fail closed: %v", err)
			}
			if _, err := os.Stat(cfg.DBPath); !os.IsNotExist(err) {
				t.Fatalf("rejected production startup touched database: %v", err)
			}
		})
	}
}

type p0FactoryGenerator struct {
	generator.Generator
	id string
}

func (g p0FactoryGenerator) ID() string { return g.id }

func TestP0ProductionFactoriesGateBeforeConstruction(t *testing.T) {
	base, err := generator.NewArithmeticBaseline(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	constructed := map[string]int{}
	var factories []generatorFactory
	for _, id := range []string{"ready", "experimental-ready", "not-ready", "unknown"} {
		factories = append(factories, generatorFactory{id: id, new: func() (generator.Generator, error) {
			constructed[id]++
			return p0FactoryGenerator{Generator: base, id: id}, nil
		}})
	}
	lifecycle := func(id string) (bool, bool) {
		switch id {
		case "ready":
			return false, true
		case "experimental-ready":
			return true, true
		case "not-ready":
			return false, false
		default:
			return true, false
		}
	}
	selected, err := selectGenerators(factories, nil, true, lifecycle)
	if err != nil || len(selected) != 1 || selected[0].ID() != "ready" || constructed["ready"] != 1 || len(constructed) != 1 {
		t.Fatalf("constructor admission: selected=%d constructed=%v error=%v", len(selected), constructed, err)
	}
	for _, allow := range [][]string{{"ready", "experimental-ready"}, {"not-ready"}, {"unknown"}, {"missing"}} {
		clear(constructed)
		if _, err := selectGenerators(factories, allow, true, lifecycle); err == nil || len(constructed) != 0 {
			t.Fatalf("invalid whitelist constructed generators: %v error=%v", constructed, err)
		}
	}
	if _, err := selectGenerators(factories, nil, true, func(string) (bool, bool) { return true, false }); err == nil {
		t.Fatal("empty production set accepted")
	}
}
