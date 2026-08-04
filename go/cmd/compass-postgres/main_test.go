//go:build unix

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
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

func TestClassifyCreatedbOutput(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want createdbOutcome
	}{
		{
			name: "already exists is idempotent success",
			out:  `createdb: error: database creation failed: ERROR:  database "compass" already exists`,
			want: createdbAlreadyExists,
		},
		{
			name: "could not connect is transient",
			out:  `createdb: error: connection to server on socket "/tmp/s/.s.PGSQL.5432" failed: No such file or directory`,
			want: createdbTransient,
		},
		{
			name: "libpq could not connect phrasing is transient",
			out:  "createdb: error: could not connect to server: Connection refused",
			want: createdbTransient,
		},
		{
			name: "starting up is transient",
			out:  `createdb: error: the database system is starting up`,
			want: createdbTransient,
		},
		{
			name: "a real error is fatal",
			out:  `createdb: error: permission denied to create database`,
			want: createdbFatal,
		},
		{
			name: "empty output is fatal",
			out:  "",
			want: createdbFatal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyCreatedbOutput(tt.out); got != tt.want {
				t.Fatalf("classifyCreatedbOutput(%q) = %d, want %d", tt.out, got, tt.want)
			}
		})
	}
}

func TestCLocaleEnv(t *testing.T) {
	// Every case must end with LC_ALL=C authoritative and no residual
	// message-affecting locale var, whatever the inherited environment.
	tests := []struct {
		name string
		env  []string
	}{
		{
			name: "empty env still pins LC_ALL=C",
			env:  nil,
		},
		{
			name: "inherited LANG is dropped",
			env:  []string{"PATH=/usr/bin", "LANG=fr_FR.UTF-8"},
		},
		{
			name: "inherited LC_MESSAGES is dropped, not duplicated",
			env:  []string{"LC_MESSAGES=fr_FR.UTF-8", "HOME=/home/u"},
		},
		{
			name: "inherited LC_ALL is overridden",
			env:  []string{"LC_ALL=de_DE.UTF-8", "TERM=xterm"},
		},
		{
			name: "all three present are all dropped",
			env:  []string{"LC_ALL=ja_JP.UTF-8", "LC_MESSAGES=ja_JP.UTF-8", "LANG=ja_JP.UTF-8", "USER=u"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cLocaleEnv(tt.env)
			// Exactly one LC_ALL entry, and it is C.
			var lcAll int
			for _, kv := range got {
				switch {
				case strings.HasPrefix(kv, "LC_ALL="):
					lcAll++
					if kv != "LC_ALL=C" {
						t.Fatalf("LC_ALL entry = %q, want LC_ALL=C", kv)
					}
				case strings.HasPrefix(kv, "LC_MESSAGES="), strings.HasPrefix(kv, "LANG="):
					t.Fatalf("residual message-locale var survived: %q", kv)
				}
			}
			if lcAll != 1 {
				t.Fatalf("LC_ALL appeared %d times; want exactly 1 (authoritative, no duplicate key)", lcAll)
			}
			// Non-locale entries are preserved.
			for _, kv := range tt.env {
				if strings.HasPrefix(kv, "LC_ALL=") ||
					strings.HasPrefix(kv, "LC_MESSAGES=") ||
					strings.HasPrefix(kv, "LANG=") {
					continue
				}
				if !slices.Contains(got, kv) {
					t.Fatalf("non-locale entry %q was dropped", kv)
				}
			}
		})
	}
}
