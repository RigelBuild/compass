package main

import "testing"

func TestVersionRequested(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"long flag alone", []string{"--version"}, true},
		{"short flag alone", []string{"-version"}, true},
		{"no args", nil, false},
		{"flag with trailing arg", []string{"--version", "extra"}, false},
		{"unrelated arg", []string{"up"}, false},
		{"help flag", []string{"--help"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := versionRequested(tt.args); got != tt.want {
				t.Fatalf("versionRequested(%q) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

func TestVersionDefault(t *testing.T) {
	if version != "0.1.0" {
		t.Fatalf("version = %q, want %q", version, "0.1.0")
	}
}
