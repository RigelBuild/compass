// Package refs parses and finds work references in text: a tracker key such
// as RIG-12 and a forge ref such as owner/repo#12. The grammar is shared with
// the UI through testdata/refs.json.
package refs

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Kind tells a tracker key from a forge ref.
type Kind int

const (
	KindTracker Kind = iota + 1
	KindForge
)

// Ref is one parsed reference. A tracker ref sets Key; a forge ref sets Owner
// and Repo, both lowercased.
type Ref struct {
	Kind   Kind
	Key    string
	Owner  string
	Repo   string
	Number uint64
}

// Span is one ref found by Scan; Start and End are byte offsets into the text.
type Span struct {
	Start, End int
	Ref        Ref
}

// ErrInvalid reports a string that is not exactly one ref.
var ErrInvalid = errors.New("refs: not a ref")

const maxKeyLen = 10

// MaxNumber is the largest ref number: it fits int64 storage and is exact in a
// JavaScript number, so Go and the UI accept the same refs.
const MaxNumber = 1<<53 - 1

// Parse reads s as exactly one ref and returns its canonical form.
func Parse(s string) (Ref, error) {
	if r, end, ok := matchAt(s, 0); ok && end == len(s) {
		return r, nil
	}
	return Ref{}, fmt.Errorf("%w: %q", ErrInvalid, s)
}

// String returns the canonical text of r.
func (r Ref) String() string {
	n := strconv.FormatUint(r.Number, 10)
	if r.Kind == KindForge {
		return r.Owner + "/" + r.Repo + "#" + n
	}
	return r.Key + "-" + n
}

// Scan returns every ref in text, in order. A ref must start and end on a
// token edge, so refs inside words and paths are not found. A
// whitespace-delimited token holding "://" is a URL and is skipped whole.
func Scan(text string) []Span {
	var spans []Span
	for i := 0; i < len(text); i++ {
		if i == 0 || isSpace(text[i-1]) {
			if end := tokenEnd(text, i); strings.Contains(text[i:end], "://") {
				i = end
				continue
			}
		}
		if i > 0 && blocksStart(text[i-1]) {
			continue
		}
		r, end, ok := matchAt(text, i)
		if !ok || (end < len(text) && blocksEnd(text[end])) {
			continue
		}
		spans = append(spans, Span{Start: i, End: end, Ref: r})
		i = end - 1
	}
	return spans
}

func tokenEnd(s string, i int) int {
	return run(s, i, func(c byte) bool { return !isSpace(c) })
}

// matchAt matches the longest ref that starts at s[i]. A forge ref is tried
// first because its owner class also matches a tracker key.
func matchAt(s string, i int) (Ref, int, bool) {
	if r, end, ok := matchForge(s, i); ok {
		return r, end, true
	}
	return matchTracker(s, i)
}

func matchForge(s string, i int) (Ref, int, bool) {
	ownerEnd := run(s, i, isNameByte)
	if ownerEnd == i || ownerEnd >= len(s) || s[ownerEnd] != '/' {
		return Ref{}, 0, false
	}
	repoEnd := run(s, ownerEnd+1, isNameByte)
	if repoEnd == ownerEnd+1 || repoEnd >= len(s) || s[repoEnd] != '#' {
		return Ref{}, 0, false
	}
	n, end, ok := number(s, repoEnd+1)
	if !ok {
		return Ref{}, 0, false
	}
	return Ref{
		Kind:   KindForge,
		Owner:  strings.ToLower(s[i:ownerEnd]),
		Repo:   strings.ToLower(s[ownerEnd+1 : repoEnd]),
		Number: n,
	}, end, true
}

func matchTracker(s string, i int) (Ref, int, bool) {
	if i >= len(s) || !isUpper(s[i]) {
		return Ref{}, 0, false
	}
	keyEnd := run(s, i+1, func(c byte) bool { return isUpper(c) || isDigit(c) })
	if keyLen := keyEnd - i; keyLen < 2 || keyLen > maxKeyLen || keyEnd >= len(s) || s[keyEnd] != '-' {
		return Ref{}, 0, false
	}
	n, end, ok := number(s, keyEnd+1)
	if !ok {
		return Ref{}, 0, false
	}
	return Ref{Kind: KindTracker, Key: s[i:keyEnd], Number: n}, end, true
}

// number reads a decimal run at s[i]. A value above MaxNumber is not a ref.
func number(s string, i int) (uint64, int, bool) {
	end := run(s, i, isDigit)
	if end == i {
		return 0, 0, false
	}
	n, err := strconv.ParseUint(s[i:end], 10, 64)
	if err != nil || n > MaxNumber {
		return 0, 0, false
	}
	return n, end, true
}

func run(s string, i int, in func(byte) bool) int {
	for i < len(s) && in(s[i]) {
		i++
	}
	return i
}

func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isWord(c byte) bool {
	return isDigit(c) || isUpper(c) || (c >= 'a' && c <= 'z') || c == '_'
}
func isNameByte(c byte) bool { return isWord(c) || c == '.' || c == '-' }
func isSpace(c byte) bool    { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// blocksStart reports a byte that joins the next one into a larger token.
func blocksStart(c byte) bool { return isNameByte(c) || c == '/' || c == '#' }

// blocksEnd reports a byte that extends a ref into a larger token.
func blocksEnd(c byte) bool { return isWord(c) || c == '/' || c == '#' || c == '-' }
