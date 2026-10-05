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

func TestAccountsVisibilityPredicateParity(t *testing.T) {
	predicates := readAccountVisibilityPredicates(t, "queries/accounts.sql")
	list := predicates["ListVisibleAccounts"]
	accountList := predicates["AccountVisibleTo"]
	globalResolver := predicates["ResolveVisibleGlobalHandles"]
	agentResolver := predicates["ResolveVisibleAgentHandles"]

	if list != accountList {
		t.Errorf("list predicates differ:\n ListVisibleAccounts: %s\n AccountVisibleTo: %s", list, accountList)
	}
	if globalResolver != agentResolver {
		t.Errorf("resolver predicates differ:\n ResolveVisibleGlobalHandles: %s\n ResolveVisibleAgentHandles: %s", globalResolver, agentResolver)
	}

	for name, resolver := range map[string]string{
		"ResolveVisibleGlobalHandles": globalResolver,
		"ResolveVisibleAgentHandles":  agentResolver,
	} {
		withoutPeering := removePeeredDisjunct(t, name, resolver)
		if withoutPeering != list {
			t.Errorf("%s without its peered disjunct differs from list predicate:\n got: %s\nwant: %s", name, withoutPeering, list)
		}
	}
}

func removePeeredDisjunct(t *testing.T, name, predicate string) string {
	t.Helper()
	const marker = "OR EXISTS ( SELECT 1 FROM user_peers p_out"
	if count := strings.Count(predicate, marker); count != 1 {
		t.Fatalf("%s peered disjunct count = %d, want 1", name, count)
	}
	start := strings.Index(predicate, marker)
	open := start + strings.Index(predicate[start:], "EXISTS ") + len("EXISTS ")
	depth, end := 0, -1
	for i := open; i < len(predicate); i++ {
		switch predicate[i] {
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
		t.Fatalf("%s peered disjunct has no closing parenthesis", name)
	}
	return strings.Join(strings.Fields(predicate[:start]+predicate[end:]), " ")
}

func readAccountVisibilityPredicates(t *testing.T, path string) map[string]string {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	const (
		listMarker        = "-- name: ListVisibleAccounts"
		accountListMarker = "-- name: AccountVisibleTo"
		globalMarker      = "-- name: ResolveVisibleGlobalHandles"
		agentMarker       = "-- name: ResolveVisibleAgentHandles"
	)
	queries := map[string]string{
		"ListVisibleAccounts":         listMarker,
		"AccountVisibleTo":            accountListMarker,
		"ResolveVisibleGlobalHandles": globalMarker,
		"ResolveVisibleAgentHandles":  agentMarker,
	}
	predicates := make(map[string]string, len(queries))
	for name, marker := range queries {
		markerStart := bytes.Index(source, []byte(marker))
		if markerStart < 0 {
			t.Fatalf("%s: missing query marker %q", path, marker)
		}
		queryStart := markerStart + len(marker)
		queryEnd := len(source)
		if next := bytes.Index(source[queryStart:], []byte("-- name:")); next >= 0 {
			queryEnd = queryStart + next
		}
		var starts []int
		for offset := queryStart; offset < queryEnd; {
			relative := bytes.Index(source[offset:queryEnd], []byte("WHERE ("))
			if relative < 0 {
				break
			}
			starts = append(starts, offset+relative+len("WHERE "))
			offset += relative + len("WHERE (")
		}
		if len(starts) == 0 {
			t.Fatalf("%s: %s has no parenthesized visibility predicate", path, name)
		}
		start := starts[len(starts)-1]
		depth, end := 0, -1
		for i := start; i < queryEnd; i++ {
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
			t.Fatalf("%s: %s visibility predicate has no closing parenthesis", path, name)
		}
		predicates[name] = strings.Join(strings.Fields(string(source[start:end])), " ")
	}
	return predicates
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
