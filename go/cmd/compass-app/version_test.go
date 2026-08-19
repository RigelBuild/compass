package main

import (
	"strings"
	"testing"
)

func TestPrintVersionIfRequested(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantHandled bool
		wantOut     string
	}{
		{"long flag alone", []string{"--version"}, true, version + "\n"},
		{"short flag alone", []string{"-version"}, true, version + "\n"},
		{"no args", nil, false, ""},
		{"flag with trailing arg", []string{"--version", "extra"}, false, ""},
		{"unrelated arg", []string{"up"}, false, ""},
		{"help flag", []string{"--help"}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			handled, err := printVersionIfRequested(tt.args, &out)
			if err != nil {
				t.Fatalf("printVersionIfRequested(%q) err = %v, want nil", tt.args, err)
			}
			if handled != tt.wantHandled {
				t.Errorf("handled = %v, want %v", handled, tt.wantHandled)
			}
			if out.String() != tt.wantOut {
				t.Errorf("output = %q, want %q", out.String(), tt.wantOut)
			}
		})
	}
}
