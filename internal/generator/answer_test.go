package generator

import "testing"

func TestNormalizeAngle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"zero", "0", "0", true},
		{"plain", "375", "375", true},
		{"leading zero", "0375", "375", true},
		{"outer space", " 375 ", "375", true},
		{"max", "3599", "3599", true},
		{"over max", "3600", "", false},
		{"negative", "-1", "", false},
		{"plus", "+90", "", false},
		{"decimal", "37.5", "", false},
		{"internal space", "3 75", "", false},
		{"empty", "", "", false},
		{"fullwidth", "３７５", "", false},
		{"too long", "00000", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NormalizeAngle(tc.in)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("NormalizeAngle(%q) = (%q,%v)，期望 (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestNormalizePoint(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"origin", "0,0", "0,0", true},
		{"max", "999,999", "999,999", true},
		{"canonical spaces", " 12 , 034 ", "12,34", true},
		{"leading zeros", "007,009", "7,9", true},
		{"over x", "1000,0", "", false},
		{"over y", "0,1000", "", false},
		{"one field", "12", "", false},
		{"three fields", "1,2,3", "", false},
		{"decimal", "1.0,2", "", false},
		{"negative", "-1,2", "", false},
		{"empty field", "1,", "", false},
		{"internal space", "1 2,3", "", false},
		{"empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NormalizePoint(tc.in)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("NormalizePoint(%q) = (%q,%v)，期望 (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestNormalizeTiming(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"zero", "0", "0", true},
		{"leading zero", "0600", "600", true},
		{"outer space", " 60000 ", "60000", true},
		{"max", "60000", "60000", true},
		{"over max", "60001", "", false},
		{"negative", "-1", "", false},
		{"decimal", "1.5", "", false},
		{"empty", "", "", false},
		{"plus", "+1", "", false},
		{"too long", "000000", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NormalizeTiming(tc.in)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("NormalizeTiming(%q) = (%q,%v)，期望 (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestFormatPrimitiveAnswers(t *testing.T) {
	if got, ok := FormatAngle(375); !ok || got != "375" {
		t.Fatalf("FormatAngle(375) = (%q,%v)", got, ok)
	}
	if _, ok := FormatAngle(3600); ok {
		t.Fatal("FormatAngle(3600) 应拒绝")
	}
	if got, ok := FormatPoint(12, 34); !ok || got != "12,34" {
		t.Fatalf("FormatPoint(12,34) = (%q,%v)", got, ok)
	}
	if _, ok := FormatPoint(1000, 0); ok {
		t.Fatal("FormatPoint(1000,0) 应拒绝")
	}
	if got, ok := FormatTiming(60000); !ok || got != "60000" {
		t.Fatalf("FormatTiming(60000) = (%q,%v)", got, ok)
	}
	if _, ok := FormatTiming(60001); ok {
		t.Fatal("FormatTiming(60001) 应拒绝")
	}
}

func TestNormalizePrimitiveAnswerDispatch(t *testing.T) {
	for _, tc := range []struct {
		typ, in, want string
	}{
		{AnswerTypeAngle, "375", "375"},
		{AnswerTypePoint, "1,2", "1,2"},
		{AnswerTypeTiming, "1200", "1200"},
	} {
		got, ok := NormalizePrimitiveAnswer(tc.typ, tc.in)
		if !ok || got != tc.want {
			t.Fatalf("NormalizePrimitiveAnswer(%q,%q) = (%q,%v)", tc.typ, tc.in, got, ok)
		}
	}
	for _, typ := range []string{AnswerTypeText, AnswerTypeInteger, AnswerTypeCells, AnswerTypePairs, "path", ""} {
		if _, ok := NormalizePrimitiveAnswer(typ, "1"); ok {
			t.Fatalf("NormalizePrimitiveAnswer(%q) 不应接受通用原语", typ)
		}
	}
}

func TestKnownAnswerType(t *testing.T) {
	for _, typ := range []string{
		AnswerTypeText, AnswerTypeInteger, AnswerTypeCells, AnswerTypePairs,
		AnswerTypeAngle, AnswerTypePoint, AnswerTypeTiming,
	} {
		if !KnownAnswerType(typ) {
			t.Fatalf("KnownAnswerType(%q) = false", typ)
		}
		if err := ValidateAnswerType(typ); err != nil {
			t.Fatalf("ValidateAnswerType(%q): %v", typ, err)
		}
	}
	for _, typ := range []string{"", "path", "order", "polygon", "float"} {
		if KnownAnswerType(typ) {
			t.Fatalf("KnownAnswerType(%q) = true（未实现类型不得被当成已知）", typ)
		}
		if err := ValidateAnswerType(typ); err == nil {
			t.Fatalf("ValidateAnswerType(%q) 应报错", typ)
		}
	}
}
