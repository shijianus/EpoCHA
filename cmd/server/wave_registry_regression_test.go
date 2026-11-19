package main

import (
	"bytes"
	"testing"

	"github.com/shijianus/EpoCHA/internal/generator"
)

func TestKnownGeneratorsIncludesWaveGenerators(t *testing.T) {
	known, err := knownGenerators(bytes.Repeat([]byte{0x5a}, 32))
	if err != nil {
		t.Fatalf("knownGenerators: %v", err)
	}

	registry := generator.NewRegistry()
	for _, g := range known {
		if err := registry.Register(g); err != nil {
			t.Fatalf("Register(%q): %v", g.ID(), err)
		}
	}

	want := []struct {
		id      string
		version string
		media   string
	}{
		{generator.TileRotationID, "v1", generator.MediaImage},
		{generator.SymbolPathID, "v1", generator.MediaImage},
		{generator.CrowdFindID, "v1", generator.MediaImage},
		{generator.VisualRankID, "v1", generator.MediaImage},
		{generator.ProgressiveTargetID, "v1", generator.MediaImage},
		{generator.ShellTrackID, "v1", generator.MediaImage},
		{generator.SilhouetteAssemblyID, "v1", generator.MediaImage},
	}

	for _, tc := range want {
		t.Run(tc.id, func(t *testing.T) {
			g, err := registry.Lookup(tc.id)
			if err != nil {
				t.Fatalf("knownGenerators registry.Lookup(%q): %v", tc.id, err)
			}
			if g.ID() != tc.id || g.Version() != tc.version || g.Media() != tc.media {
				t.Fatalf("registry metadata = (%q, %q, %q), want (%q, %q, %q)",
					g.ID(), g.Version(), g.Media(), tc.id, tc.version, tc.media)
			}
		})
	}
}
