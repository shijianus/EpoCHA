package risk

import (
	"errors"
	"testing"
)

func TestP0TrustedProxyRejectsUniversalNetworks(t *testing.T) {
	for _, nets := range [][]string{
		{"0.0.0.0/0"}, {"::/0"}, {"192.0.2.1/0"}, {"2001:db8::1/0"},
		{"192.0.2.0/24", "0.0.0.0/0"}, {"::/0", "2001:db8::/32"},
	} {
		for _, trust := range []bool{false, true} {
			t.Run(nets[0]+"/trust="+map[bool]string{false: "false", true: "true"}[trust], func(t *testing.T) {
				s, err := NewSource(trust, nets)
				if s != nil || !errors.Is(err, ErrBadConfig) {
					t.Fatalf("universal proxy accepted: source=%v error=%v", s != nil, err)
				}
			})
		}
	}
	for _, nets := range [][]string{{"192.0.2.0/24"}, {"2001:db8::/32"}, {"192.0.2.1/32", "::1/128"}} {
		if _, err := NewSource(true, nets); err != nil {
			t.Fatalf("bounded proxy rejected: %v", err)
		}
	}
}
