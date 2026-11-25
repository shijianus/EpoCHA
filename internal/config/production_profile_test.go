package config

import (
	"errors"
	"strings"
	"testing"
)

func TestP0ProductionProfileConfiguration(t *testing.T) {
	defaults, err := Load(nil, envMap(baseEnv(t)))
	if err != nil || defaults.ProductionProfile {
		t.Fatalf("default profile: error=%v", err)
	}
	for _, value := range []string{"true", "false"} {
		t.Run(value, func(t *testing.T) {
			env := baseEnv(t)
			env["EPOCHA_PRODUCTION_PROFILE"] = value
			cfg, err := Load(nil, envMap(env))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(cfg.SafeSummary(), "production_profile="+value) {
				t.Fatal("production profile is silently ignored")
			}
		})
	}
	for _, value := range []string{"tru", "yes", "invalid"} {
		t.Run(value, func(t *testing.T) {
			env := baseEnv(t)
			env["EPOCHA_PRODUCTION_PROFILE"] = value
			if _, err := Load(nil, envMap(env)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid production profile accepted: %v", err)
			}
		})
	}
}
