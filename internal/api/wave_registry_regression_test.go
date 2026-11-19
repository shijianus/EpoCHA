package api

import (
	"testing"

	"github.com/shijianus/EpoCHA/internal/generator"
)

func TestTestRegistryIncludesWaveGenerators(t *testing.T) {
	registry := testRegistry(t, testMasterKey)
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
				t.Fatalf("testRegistry.Lookup(%q): %v", tc.id, err)
			}
			if g.ID() != tc.id || g.Version() != tc.version || g.Media() != tc.media {
				t.Fatalf("registry metadata = (%q, %q, %q), want (%q, %q, %q)",
					g.ID(), g.Version(), g.Media(), tc.id, tc.version, tc.media)
			}
		})
	}
}
