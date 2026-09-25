package auth

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestClaimValues(t *testing.T) {
	tests := []struct {
		name string
		give map[string]any
		want []string
	}{
		{name: "list", give: map[string]any{"groups": []any{"a", "Pulumi Viewers"}},
			want: []string{"a", "Pulumi Viewers"}},
		{name: "string", give: map[string]any{"groups": "a"}, want: []string{"a"}},
		{name: "absent", give: map[string]any{}, want: nil},
		{name: "nil map", give: nil, want: nil},
		{name: "wrong type", give: map[string]any{"groups": 3.0}, want: nil},
		{name: "mixed list drops non-strings", give: map[string]any{"groups": []any{"a", 1.0}},
			want: []string{"a"}},
		{name: "string slice", give: map[string]any{"groups": []string{"a", "b"}},
			want: []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, ClaimValues(tt.give, "groups")); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}
}

func TestAllowed(t *testing.T) {
	tests := []struct {
		name      string
		giveVals  []string
		giveAllow []string
		want      bool
	}{
		{name: "match", giveVals: []string{"x", "Pulumi Viewers"},
			giveAllow: []string{"Pulumi Viewers"}, want: true},
		{name: "case sensitive", giveVals: []string{"pulumi viewers"},
			giveAllow: []string{"Pulumi Viewers"}, want: false},
		{name: "no values", giveVals: nil, giveAllow: []string{"a"}, want: false},
		{name: "empty allowlist denies", giveVals: []string{"a"}, giveAllow: nil, want: false},
		{name: "empty value never matches", giveVals: []string{""}, giveAllow: []string{""},
			want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Allowed(tt.giveVals, tt.giveAllow); got != tt.want {
				t.Fatalf("Allowed = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSafeReturn(t *testing.T) {
	tests := []struct{ give, want string }{
		{"/stacks/a/b", "/stacks/a/b"},
		{"/stacks/a/b?before=1.2", "/stacks/a/b?before=1.2"},
		{"", "/"},
		{"//evil.example", "/"},
		{"/\\evil.example", "/"},
		{"https://evil.example", "/"},
		{"stacks", "/"},
		{"/%2F%2Fevil.example", "/%2F%2Fevil.example"},
	}
	for _, tt := range tests {
		if got := safeReturn(tt.give); got != tt.want {
			t.Errorf("safeReturn(%q) = %q, want %q", tt.give, got, tt.want)
		}
	}
}
