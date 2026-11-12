package generator

import "testing"

func TestNormalizeMasks(t *testing.T) {
	valid := []struct {
		in   string
		want string
	}{
		{"v1:1a2b3c4d5", "v1:1a2b3c4d5"},
		{"  V1:1A2B3C4D5\t", "v1:1a2b3c4d5"},
		{"v1:000000000", "v1:000000000"},
	}
	for _, tc := range valid {
		got, ok := NormalizeMasks(tc.in, 9)
		if !ok || got != tc.want {
			t.Fatalf("NormalizeMasks(%q) = (%q,%v)，期望 (%q,true)", tc.in, got, ok, tc.want)
		}
	}
	invalid := []string{
		"", "v1:", "v1:1a2b3c4d", "v1:1a2b3c4d55", "v1:1a2b3c4d5x",
		"v2:1a2b3c4d5", "1a2b3c4d5", "v1:1a2b3c4d 5", "v1:１a2b3c4d5",
		"v1:1a2b3c4d5trailing", "v1:-1a2b3c4d5",
	}
	for _, in := range invalid {
		if got, ok := NormalizeMasks(in, 9); ok {
			t.Fatalf("NormalizeMasks(%q) 应拒绝，得到 %q", in, got)
		}
	}
}

func TestNormalizePathEncoding(t *testing.T) {
	valid := []struct {
		in   string
		want string
	}{
		{"v1:03.09.10.16.22.28", "v1:03.09.10.16.22.28"},
		{" V1:00.01.02 ", "v1:00.01.02"},
	}
	for _, tc := range valid {
		got, ok := NormalizePath(tc.in, 36, 3, 8)
		if !ok || got != tc.want {
			t.Fatalf("NormalizePath(%q) = (%q,%v)，期望 (%q,true)", tc.in, got, ok, tc.want)
		}
	}
	invalid := []string{
		"", "v1:", "v1:03.09", "v1:03.09.10.16.22.28.30.31.32",
		"v1:3.09.10", "v1:03.09.10.", "v1:.03.09.10", "v1:03..09.10",
		"v1:03.09.10.16.40", "v1:03.09.10.16.22.22",
		"v1:03.09.10.16.22.28x", "v2:03.09.10", "v1:-3.09.10", "v1:０3.09.10",
	}
	for _, in := range invalid {
		if got, ok := NormalizePath(in, 36, 3, 8); ok {
			t.Fatalf("NormalizePath(%q) 应拒绝，得到 %q", in, got)
		}
	}
}

func TestNormalizeSelectEncoding(t *testing.T) {
	valid := []struct {
		in   string
		want string
	}{
		{"v1:0", "v1:0"}, {"v1:35", "v1:35"}, {"  V1:7 ", "v1:7"},
	}
	for _, tc := range valid {
		got, ok := NormalizeSelect(tc.in, 36)
		if !ok || got != tc.want {
			t.Fatalf("NormalizeSelect(%q) = (%q,%v)，期望 (%q,true)", tc.in, got, ok, tc.want)
		}
	}
	invalid := []string{
		"", "v1:", "v1:36", "v1:-1", "v1:01", "v1:1.0", "v1:1x",
		"v2:1", "v1:１", "v1:1trailing",
	}
	for _, in := range invalid {
		if got, ok := NormalizeSelect(in, 36); ok {
			t.Fatalf("NormalizeSelect(%q) 应拒绝，得到 %q", in, got)
		}
	}
}

func TestNormalizeOrderEncoding(t *testing.T) {
	valid := []struct {
		in   string
		want string
	}{
		{"v1:A.B.C.D.E", "v1:A.B.C.D.E"},
		{"v1:e.d.c.b.a", "v1:E.D.C.B.A"},
		{" V1:A.B.C.D.E ", "v1:A.B.C.D.E"},
	}
	for _, tc := range valid {
		got, ok := NormalizeOrder(tc.in, 5)
		if !ok || got != tc.want {
			t.Fatalf("NormalizeOrder(%q) = (%q,%v)，期望 (%q,true)", tc.in, got, ok, tc.want)
		}
	}
	invalid := []string{
		"", "v1:", "v1:A.B.C.D", "v1:A.B.C.D.E.F", "v1:A.B.C.D.D",
		"v1:A.B.C.D.1", "v1:AA.B.C.D.E", "v1:A.B.C.D.E.", "v1:.A.B.C.D.E",
		"v1:A,B,C,D,E", "v2:A.B.C.D.E", "v1:Ａ.B.C.D.E",
	}
	for _, in := range invalid {
		if got, ok := NormalizeOrder(in, 5); ok {
			t.Fatalf("NormalizeOrder(%q) 应拒绝，得到 %q", in, got)
		}
	}
}

func TestNormalizeSequenceEncoding(t *testing.T) {
	valid := []struct {
		in   string
		want string
	}{
		{"v1:0.4.8", "v1:0.4.8"},
		{"v1:3.3.0", "v1:3.3.0"},
		{" V1:2.0.3 ", "v1:2.0.3"},
	}
	for _, tc := range valid {
		got, ok := NormalizeSequence(tc.in, 3, 8)
		if !ok || got != tc.want {
			t.Fatalf("NormalizeSequence(%q) = (%q,%v)，期望 (%q,true)", tc.in, got, ok, tc.want)
		}
	}
	invalid := []string{
		"", "v1:", "v1:0.4", "v1:0.4.8.1", "v1:0..8", "v1:0.4.",
		"v1:.0.4", "v1:9.4.8", "v1:0.4.10", "v1:-1.4.8", "v1:0,4,8",
		"v2:0.4.8", "v1:０.4.8", "v1:0.4.8x",
	}
	for _, in := range invalid {
		if got, ok := NormalizeSequence(in, 3, 8); ok {
			t.Fatalf("NormalizeSequence(%q) 应拒绝，得到 %q", in, got)
		}
	}
}

func TestNormalizePlacementsEncoding(t *testing.T) {
	valid := []struct {
		in   string
		want string
	}{
		{"v1:0:2,3:1,4:0,1:3,2:0", "v1:0:2,3:1,4:0,1:3,2:0"},
		{" V1:4:3,3:2,2:1,1:0,0:3 ", "v1:4:3,3:2,2:1,1:0,0:3"},
	}
	for _, tc := range valid {
		got, ok := NormalizePlacements(tc.in, 5, 5)
		if !ok || got != tc.want {
			t.Fatalf("NormalizePlacements(%q) = (%q,%v)，期望 (%q,true)", tc.in, got, ok, tc.want)
		}
	}
	invalid := []string{
		"", "v1:", "v1:0:2,3:1,4:0,1:3", "v1:0:2,3:1,4:0,1:3,2:0,4:0",
		"v1:0:2,0:1,4:0,1:3,2:0", "v1:5:2,3:1,4:0,1:3,2:0",
		"v1:0:4,3:1,4:0,1:3,2:0", "v1:0:2,3:1,4:0,1:3,2", "v1:0-2,3:1,4:0,1:3,2:0",
		"v1:0:2,3:1,4:0,1:3,2:0extra", "v2:0:2,3:1,4:0,1:3,2:0", "v1:０:2,3:1,4:0,1:3,2:0",
	}
	for _, in := range invalid {
		if got, ok := NormalizePlacements(in, 5, 5); ok {
			t.Fatalf("NormalizePlacements(%q) 应拒绝，得到 %q", in, got)
		}
	}
}

func FuzzNormalizeNewEncodings(f *testing.F) {
	for _, s := range []string{
		"", "v1:", "v1:1a2b3c4d5", "v1:03.09.10", "v1:35", "v1:A.B.C.D.E",
		"v1:0.4.8", "v1:0:2,3:1,4:0,1:3,2:0", "v1:\xff\xfe",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		NormalizeMasks(s, 9)
		NormalizePath(s, 36, 3, 8)
		NormalizeSelect(s, 36)
		NormalizeOrder(s, 5)
		NormalizeSequence(s, 3, 8)
		NormalizePlacements(s, 5, 5)
	})
}
