package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/shijianus/EpoCHA/internal/store"
)

func TestLedgerConfigDefaultsAndIndependentBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  int
	}{
		{"default", "", 100000},
		{"below-active", "1", 1},
		{"explicit", "12345", 12345},
		{"maximum", "1000000", 1000000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := baseEnv(t)
			env["EPOCHA_MAX_LEDGER_ROWS"] = tc.value
			cfg, err := Load(nil, envMap(env))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.MaxLedgerRows != tc.want {
				t.Errorf("ledger cap=%d, want %d", cfg.MaxLedgerRows, tc.want)
			}
			if cfg.MaxActiveChallenges != 10000 || cfg.MaxActivePerSite != 2000 {
				t.Errorf("total cap must not alter active limits: %d/%d", cfg.MaxActiveChallenges, cfg.MaxActivePerSite)
			}
			if tc.value == "" && cfg.MaxLedgerRows != store.DefaultMaxLedgerRows {
				t.Errorf("config and direct-store default differ: %d/%d", cfg.MaxLedgerRows, store.DefaultMaxLedgerRows)
			}
		})
	}
}

func TestLedgerConfigRejectsInvalidExplicitValues(t *testing.T) {
	for _, value := range []string{"0", "-1", "1000001", "1.5", "not-a-number", "999999999999999999999999"} {
		t.Run(value, func(t *testing.T) {
			env := baseEnv(t)
			env["EPOCHA_MAX_LEDGER_ROWS"] = value
			_, err := Load(nil, envMap(env))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "EPOCHA_MAX_LEDGER_ROWS") {
				t.Fatalf("invalid total cap must reject with the config key, got %v", err)
			}
		})
	}
}

func TestLedgerConfigSafeSummaryIncludesSeparateCap(t *testing.T) {
	env := baseEnv(t)
	env["EPOCHA_MAX_LEDGER_ROWS"] = "17"
	cfg, err := Load(nil, envMap(env))
	if err != nil {
		t.Fatal(err)
	}
	summary := cfg.SafeSummary()
	for _, field := range []string{"max_ledger_rows=17", "max_active=10000", "max_active_per_site=2000"} {
		if !strings.Contains(summary, field) {
			t.Errorf("safe summary missing %s: %s", field, summary)
		}
	}
}
