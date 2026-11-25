package generator

import (
	"bytes"
	"reflect"
	"testing"
)

type p0LifecycleStub struct {
	Generator
	id string
}

func (g p0LifecycleStub) ID() string { return g.id }

func TestP0ProductionRegistryFiltersLifecycle(t *testing.T) {
	base, err := NewArithmeticBaseline(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	for _, id := range []string{"ready", "experimental-ready", "experimental", "not-ready", "unknown"} {
		if err := r.Register(p0LifecycleStub{Generator: base, id: id}); err != nil {
			t.Fatal(err)
		}
	}
	gate, ok := any(r).(interface {
		requireProduction(func(string) (bool, bool)) error
	})
	if !ok {
		t.Fatal("registry has no injectable lifecycle admission gate")
	}
	lifecycle := func(id string) (bool, bool) {
		switch id {
		case "ready", "late-ready":
			return false, true
		case "experimental-ready":
			return true, true
		case "not-ready":
			return false, false
		default:
			return true, false
		}
	}
	if err := gate.requireProduction(lifecycle); err != nil {
		t.Fatal(err)
	}
	if got := r.IDs(); !reflect.DeepEqual(got, []string{"ready"}) {
		t.Fatalf("filtered IDs=%v", got)
	}
	for _, id := range []string{"experimental-ready", "experimental", "not-ready", "unknown"} {
		if _, err := r.Lookup(id); err == nil {
			t.Errorf("ineligible generator dispatchable: %s", id)
		}
		if err := r.Register(p0LifecycleStub{Generator: base, id: id}); err == nil {
			t.Errorf("ineligible late registration accepted: %s", id)
		}
	}
	if err := r.Register(p0LifecycleStub{Generator: base, id: "late-ready"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Lookup("late-ready"); err != nil {
		t.Fatal(err)
	}
}

// The interface assertion keeps the red snapshot compilable, so the missing
// policy produces an executed assertion failure rather than a build failure.
func TestP0ProductionRegistryRejectsExperimental(t *testing.T) {
	r := NewRegistry()
	g, err := NewArithmeticBaseline(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register(g); err != nil {
		t.Fatal(err)
	}
	gate, ok := any(r).(interface{ RequireProduction() error })
	if !ok {
		t.Fatal("registry has no production admission gate")
	}
	if err := gate.RequireProduction(); err == nil {
		t.Fatal("all-experimental registry admitted to production")
	}
	if _, err := r.Lookup(g.ID()); err == nil {
		t.Fatal("experimental generator remains dispatchable after rejection")
	}
	if err := r.Register(g); err == nil {
		t.Fatal("late experimental registration bypassed production policy")
	}
}
