package refs

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
)

type fixture struct {
	Accepted []struct {
		Input     string `json:"input"`
		Canonical string `json:"canonical"`
		Kind      string `json:"kind"`
		Key       string `json:"key"`
		Owner     string `json:"owner"`
		Repo      string `json:"repo"`
		Number    uint64 `json:"number"`
	} `json:"accepted"`
	Rejected []string `json:"rejected"`
	Scan     []struct {
		Text  string `json:"text"`
		Spans []struct {
			Start int    `json:"start"`
			End   int    `json:"end"`
			Ref   string `json:"ref"`
		} `json:"spans"`
	} `json:"scan"`
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/refs.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Accepted) == 0 || len(f.Rejected) == 0 || len(f.Scan) == 0 {
		t.Fatal("fixture has an empty section")
	}
	return f
}

var kindNames = map[string]Kind{"tracker": KindTracker, "forge": KindForge}

func TestParseAccepted(t *testing.T) {
	for _, c := range loadFixture(t).Accepted {
		t.Run(c.Input, func(t *testing.T) {
			got, err := Parse(c.Input)
			if err != nil {
				t.Fatalf("Parse(%q): %v", c.Input, err)
			}
			want := Ref{Kind: kindNames[c.Kind], Key: c.Key, Owner: c.Owner, Repo: c.Repo, Number: c.Number}
			if got != want {
				t.Fatalf("Parse(%q) = %+v, want %+v", c.Input, got, want)
			}
			if got.String() != c.Canonical {
				t.Fatalf("String() = %q, want %q", got.String(), c.Canonical)
			}
			again, err := Parse(got.String())
			if err != nil || again != got {
				t.Fatalf("canonical %q does not round-trip: %+v, %v", got.String(), again, err)
			}
		})
	}
}

func TestParseRejected(t *testing.T) {
	for _, in := range loadFixture(t).Rejected {
		t.Run(in, func(t *testing.T) {
			got, err := Parse(in)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Parse(%q) = %+v, %v; want ErrInvalid", in, got, err)
			}
		})
	}
}

func TestScan(t *testing.T) {
	for _, c := range loadFixture(t).Scan {
		t.Run(c.Text, func(t *testing.T) {
			got := Scan(c.Text)
			if len(got) != len(c.Spans) {
				t.Fatalf("Scan(%q) = %+v, want %d spans", c.Text, got, len(c.Spans))
			}
			for i, want := range c.Spans {
				g := got[i]
				if g.Start != want.Start || g.End != want.End || g.Ref.String() != want.Ref {
					t.Fatalf("span %d = {%d %d %s}, want {%d %d %s}", i, g.Start, g.End, g.Ref, want.Start, want.End, want.Ref)
				}
				if parsed, err := Parse(c.Text[g.Start:g.End]); err != nil || parsed != g.Ref {
					t.Fatalf("span %d text %q does not parse to its ref: %+v, %v", i, c.Text[g.Start:g.End], parsed, err)
				}
			}
		})
	}
}
