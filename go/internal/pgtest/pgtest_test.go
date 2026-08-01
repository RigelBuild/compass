//go:build pgtest

package pgtest

import "testing"

func TestDecideDSNSource(t *testing.T) {
	tests := []struct {
		name         string
		dsn          string
		useContainer string
		cli          string
		want         dsnSource
	}{
		{
			name: "dsn set uses shared schema",
			dsn:  "postgres://localhost/db",
			want: sourceSharedSchema,
		},
		{
			name: "no dsn no runtime skips",
			cli:  "",
			want: sourceSkipNoRuntime,
		},
		{
			name: "no dsn runtime present no opt-in fails",
			cli:  "podman",
			want: sourceFailMisconfigured,
		},
		{
			name:         "no dsn runtime present opt-in uses container",
			cli:          "podman",
			useContainer: "1",
			want:         sourceContainer,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideDSNSource(tt.dsn, tt.useContainer, tt.cli); got != tt.want {
				t.Errorf("decideDSNSource(%q, %q, %q) = %d, want %d",
					tt.dsn, tt.useContainer, tt.cli, got, tt.want)
			}
		})
	}
}
