//go:build unix

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseKeywordValueDSN(t *testing.T) {
	tests := []struct {
		name    string
		dsn     string
		want    map[string]string
		wantErr bool
	}{
		{
			name: "supervisor DSN",
			dsn:  "host=/tmp/s port=5432 dbname=compass sslmode=disable",
			want: map[string]string{
				"host": "/tmp/s", "port": "5432", "dbname": "compass", "sslmode": "disable",
			},
		},
		{
			name: "extra whitespace tolerated",
			dsn:  "  host=/tmp/s   port=5432\tdbname=compass  ",
			want: map[string]string{
				"host": "/tmp/s", "port": "5432", "dbname": "compass",
			},
		},
		{
			name: "empty value tolerated",
			dsn:  "host=/tmp/s port=5432 dbname=",
			want: map[string]string{
				"host": "/tmp/s", "port": "5432", "dbname": "",
			},
		},
		{
			name:    "mangled pair without equals",
			dsn:     "host=/tmp/s port5432 dbname=compass",
			wantErr: true,
		},
		{
			name:    "pair with empty key",
			dsn:     "host=/tmp/s =5432",
			wantErr: true,
		},
		{
			name:    "empty DSN",
			dsn:     "   ",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseKeywordValueDSN(tt.dsn)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseKeywordValueDSN(%q) = %v, want error", tt.dsn, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseKeywordValueDSN(%q) unexpected error: %v", tt.dsn, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseKeywordValueDSN(%q) = %v, want %v", tt.dsn, got, tt.want)
			}
		})
	}
}

func TestNewPGConfig(t *testing.T) {
	tests := []struct {
		name     string
		stateDir string
		dsn      string
		want     pgConfig
		wantErr  bool
	}{
		{
			name:     "assembled from state dir and DSN",
			stateDir: "/x",
			dsn:      "host=/tmp/sock port=5432 dbname=compass sslmode=disable",
			want: pgConfig{
				DataDir:   filepath.Join("/x", "postgres"),
				SocketDir: "/tmp/sock",
				Port:      "5432",
				DBName:    "compass",
			},
		},
		{
			name:     "missing host errors",
			stateDir: "/x",
			dsn:      "port=5432 dbname=compass",
			wantErr:  true,
		},
		{
			name:     "missing port errors",
			stateDir: "/x",
			dsn:      "host=/tmp/sock dbname=compass",
			wantErr:  true,
		},
		{
			name:     "missing dbname errors",
			stateDir: "/x",
			dsn:      "host=/tmp/sock port=5432",
			wantErr:  true,
		},
		{
			name:     "malformed DSN propagates parser error",
			stateDir: "/x",
			dsn:      "host=/tmp/sock bogus",
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := newPGConfig(tt.stateDir, tt.dsn)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("newPGConfig(%q, %q) = %+v, want error", tt.stateDir, tt.dsn, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("newPGConfig(%q, %q) unexpected error: %v", tt.stateDir, tt.dsn, err)
			}
			if got != tt.want {
				t.Fatalf("newPGConfig(%q, %q) = %+v, want %+v", tt.stateDir, tt.dsn, got, tt.want)
			}
		})
	}
}

func TestResolveDSN(t *testing.T) {
	const envKey = "COMPASS_DATABASE_DSN"

	t.Run("flag wins over env", func(t *testing.T) {
		t.Setenv(envKey, "host=/env port=1 dbname=e")
		got, err := resolveDSN("host=/flag port=2 dbname=f")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "host=/flag port=2 dbname=f" {
			t.Fatalf("resolveDSN = %q, want the flag value", got)
		}
	})

	t.Run("empty flag falls back to env", func(t *testing.T) {
		t.Setenv(envKey, "host=/env port=1 dbname=e")
		got, err := resolveDSN("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "host=/env port=1 dbname=e" {
			t.Fatalf("resolveDSN = %q, want the env value", got)
		}
	})

	t.Run("both empty errors", func(t *testing.T) {
		t.Setenv(envKey, "")
		if _, err := resolveDSN(""); err == nil {
			t.Fatal("resolveDSN(\"\") with empty env = nil error, want error")
		}
	})
}

func TestClusterInitialized(t *testing.T) {
	t.Run("PG_VERSION present is true", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("16\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if !clusterInitialized(dir) {
			t.Fatal("clusterInitialized = false, want true when PG_VERSION exists")
		}
	})

	t.Run("PG_VERSION absent is false", func(t *testing.T) {
		if clusterInitialized(t.TempDir()) {
			t.Fatal("clusterInitialized = true, want false for an empty dir")
		}
	})
}
