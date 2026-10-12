//go:build unix

package main

import (
	"reflect"
	"testing"

	"github.com/RigelBuild/compass/go/server"
)

func TestParseMergeQueues(t *testing.T) {
	configs, err := parseMergeQueues("Owner/Repo=trunk:main, other/project=trunk:release/next")
	if err != nil {
		t.Fatalf("parseMergeQueues: %v", err)
	}
	want := []server.MergeQueueConfig{
		{Repo: "owner/repo", Provider: "trunk", Branch: "main"},
		{Repo: "other/project", Provider: "trunk", Branch: "release/next"},
	}
	if !reflect.DeepEqual(configs, want) {
		t.Fatalf("parseMergeQueues = %#v, want %#v", configs, want)
	}
}

func TestParseMergeQueuesRejectsInvalidEntries(t *testing.T) {
	for _, raw := range []string{
		"owner=trunk:main",
		"owner/repo=trunk:",
		"owner/repo=github:main",
		"owner/repo=trunk:main,owner/repo=trunk:other",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseMergeQueues(raw); err == nil {
				t.Fatalf("parseMergeQueues(%q) returned no error", raw)
			}
		})
	}
}
