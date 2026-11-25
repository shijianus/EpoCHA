package api

import (
	"encoding/json"
	"testing"

	"github.com/shijianus/EpoCHA/internal/token"
)

func TestP0ProductionAPIRejectsUnrestrictedRegistry(t *testing.T) {
	h := newHarness(t, nil)
	// JSON sets the future flag while allowing this regression to run on the
	// original Config type, where the unknown field is silently ignored.
	if err := json.Unmarshal([]byte(`{"ProductionProfile":true}`), h.cfg); err != nil {
		t.Fatal(err)
	}
	signer, err := token.NewSigner(h.cfg.MasterKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(h.cfg, h.db, signer, h.gens, nil)
	if srv != nil || err == nil {
		t.Fatal("production API admitted an all-experimental registry")
	}
}
