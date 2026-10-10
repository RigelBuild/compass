package store

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestDeliveryReachPredicateParity(t *testing.T) {
	reads := readReachPredicates(t, "queries/delivery_reads.sql")
	cursors := readReachPredicates(t, "queries/delivery_cursors.sql")
	if len(reads) != 2 {
		t.Fatalf("delivery_reads.sql reach predicates = %d, want 2", len(reads))
	}
	if len(cursors) != 3 {
		t.Fatalf("delivery_cursors.sql reach predicates = %d, want 3", len(cursors))
	}

	want := normalizeReachPredicate(reads[0])
	for i, predicate := range append(reads[1:], cursors...) {
		if got := normalizeReachPredicate(predicate); got != want {
			t.Errorf("reach predicate %d differs:\n got: %s\nwant: %s", i+2, got, want)
		}
	}
}

func readReachPredicates(t *testing.T, path string) []string {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	const marker = "-- reach:"
	var predicates []string
	for offset := 0; offset < len(source); {
		relative := bytes.Index(source[offset:], []byte(marker))
		if relative < 0 {
			break
		}
		markerStart := offset + relative
		lineEnd := bytes.IndexByte(source[markerStart:], '\n')
		if lineEnd < 0 {
			t.Fatalf("%s: reach marker has no predicate", path)
		}
		start := markerStart + lineEnd + 1
		for start < len(source) && (source[start] == ' ' || source[start] == '\t' || source[start] == '\r' || source[start] == '\n') {
			start++
		}
		if start == len(source) || source[start] != '(' {
			t.Fatalf("%s: reach marker is not followed by a parenthesized predicate", path)
		}

		depth := 0
		end := -1
		for i := start; i < len(source); i++ {
			switch source[i] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					end = i + 1
				}
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			t.Fatalf("%s: reach predicate has no closing parenthesis", path)
		}
		predicates = append(predicates, string(source[start:end]))
		offset = end
	}
	return predicates
}

func normalizeReachPredicate(predicate string) string {
	normalized := strings.Join(strings.Fields(predicate), " ")
	normalized = strings.ReplaceAll(normalized, "$2", "<author>")
	return strings.ReplaceAll(normalized, "m.author_account_id", "<author>")
}
