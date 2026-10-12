package store

import (
	"testing"

	"github.com/RigelBuild/compass/go/internal/store/db"
)

func TestValveCut(t *testing.T) {
	row := func(seq int64, checkpoint bool, bytes int64) db.HotTailSizesRow {
		return db.HotTailSizesRow{EntrySeq: seq, Checkpoint: checkpoint, Bytes: bytes}
	}
	cases := []struct {
		name           string
		rows           []db.HotTailSizesRow
		capBytes       int64
		wantFrom, want uint64
	}{
		{name: "under cap", rows: []db.HotTailSizesRow{row(2, false, 10), row(3, false, 10)}, capBytes: 40},
		{name: "evicts oldest until it fits", rows: []db.HotTailSizesRow{row(2, false, 40), row(3, false, 40), row(4, false, 40)}, capBytes: 40, wantFrom: 2, want: 3},
		{name: "keeps the newest row", rows: []db.HotTailSizesRow{row(2, false, 40), row(3, false, 100)}, capBytes: 40, wantFrom: 2, want: 2},
		{name: "stops before a mid-tail checkpoint", rows: []db.HotTailSizesRow{row(2, false, 40), row(3, false, 40), row(4, true, 40), row(5, false, 40)}, capBytes: 40, wantFrom: 2, want: 3},
		{name: "checkpoint first means no cut", rows: []db.HotTailSizesRow{row(2, true, 40), row(3, false, 40)}, capBytes: 40},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, upto := valveCut(tc.rows, tc.capBytes)
			if from != tc.wantFrom || upto != tc.want {
				t.Fatalf("valveCut = [%d..%d], want [%d..%d]", from, upto, tc.wantFrom, tc.want)
			}
		})
	}
}
