package admin

import (
	"context"
	"errors"
	"testing"

	"github.com/shijianus/EpoCHA/internal/store"
)

// Plain store APIs are used by bootstrap/recovery and development paths. C04
// adds actor checks only to the audited management write entry points.
func TestC04AuditedCredentialCreationStillAllowsLocalPaths(t *testing.T) {
	x := newHarness(t, nil)
	if _, _, err := x.db.AddSecret(t.Context(), 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("plain local AddSecret path changed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := x.db.NewAdminKeyWithID(ctx, "local-canceled")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("plain local NewAdminKey path changed cancellation semantics: %v", err)
	}
}
