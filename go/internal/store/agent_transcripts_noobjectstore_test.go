//go:build pgtest

package store

import (
	"fmt"
	"strings"
	"testing"
)

// With no object store configured, a flush-triggering append still commits its
// row, reports the flush error once, and the same-key retry the relay sends on
// that error dedups to success. Nothing is lost and nothing is pruned.
func TestAppendWithoutObjectStoreKeepsEntriesAndRetrySucceeds(t *testing.T) {
	cases := []struct {
		name  string
		cap   int
		setup func(t *testing.T, s *Store, sess string)
		// The append that triggers a flush attempt.
		seq        uint64
		checkpoint bool
		body       string
		wantTail   []uint64
	}{
		{
			name: "checkpoint flush",
			setup: func(t *testing.T, s *Store, sess string) {
				t.Helper()
				appendOK(t, s, sess, 1, false, `{"e":1}`)
				appendOK(t, s, sess, 2, false, `{"e":2}`)
			},
			seq: 3, checkpoint: true, body: `{"e":3}`,
			wantTail: []uint64{1, 2, 3},
		},
		{
			name: "safety valve",
			cap:  40,
			setup: func(t *testing.T, s *Store, sess string) {
				t.Helper()
				appendOK(t, s, sess, 1, true, `{"cp":1}`)
				appendOK(t, s, sess, 2, false, `{"e":2}`)
			},
			seq: 3, checkpoint: false, body: `{"pad":"` + strings.Repeat("z", 40) + `"}`,
			wantTail: []uint64{1, 2, 3},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			if tc.cap > 0 {
				s.SetSafetyValveCapBytes(tc.cap)
			}
			sess := seedSession(t, s, "noos-"+strings.ReplaceAll(tc.name, " ", ""), "sess-noos-"+strings.ReplaceAll(tc.name, " ", "-"))
			if _, err := s.BindLifetime(t.Context(), sess, sessionOwner(t, s, sess)); err != nil {
				t.Fatalf("BindLifetime: %v", err)
			}
			tc.setup(t, s, sess)

			key := fmt.Sprintf("idem-%d", tc.seq)
			err := s.AppendTranscriptEntry(t.Context(), sess, tc.seq, tc.checkpoint, tc.body, key)
			if err == nil || !strings.Contains(err.Error(), "object store not configured") {
				t.Fatalf("flush-triggering append err = %v, want object store not configured", err)
			}
			if got := hotTailSeqs(t, s, sess); !seqsEqual(got, tc.wantTail...) {
				t.Fatalf("hot tail after failed flush = %v, want %v (row committed, nothing pruned)", got, tc.wantTail)
			}
			// The relay retries a CodeInternal under the same key; dedup makes it succeed.
			if err := s.AppendTranscriptEntry(t.Context(), sess, tc.seq, tc.checkpoint, tc.body, key); err != nil {
				t.Fatalf("same-key retry = %v, want nil (dedup, no flush)", err)
			}
			if got := hotTailSeqs(t, s, sess); !seqsEqual(got, tc.wantTail...) {
				t.Fatalf("hot tail after retry = %v, want %v", got, tc.wantTail)
			}
			if man := manifestRows(t, s, sess); len(man) != 0 {
				t.Fatalf("manifest = %+v, want empty with no object store", man)
			}
		})
	}
}

func appendOK(t *testing.T, s *Store, sess string, seq uint64, checkpoint bool, body string) {
	t.Helper()
	if err := s.AppendTranscriptEntry(t.Context(), sess, seq, checkpoint, body, fmt.Sprintf("idem-%d", seq)); err != nil {
		t.Fatalf("append %d: %v", seq, err)
	}
}
