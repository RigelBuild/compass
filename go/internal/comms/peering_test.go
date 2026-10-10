package comms

import (
	"errors"
	"testing"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/internal/store"
)

// A revoke landing between the approve write and the state read leaves either
// no rows or only the peer's row; the approve must not report a peering.
func TestApprovedStateAbortsWhenOwnApprovalVanished(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state store.PeeringState
		err   error
		want  connect.Code
		ok    store.PeeringState
	}{
		{name: "no rows", err: store.ErrNotFound, want: connect.CodeAborted},
		{name: "only the peer's row", state: store.PeeringPendingIncoming, want: connect.CodeAborted},
		{name: "store fault", err: errors.New("db down"), want: connect.CodeInternal},
		{name: "pending outgoing", state: store.PeeringPendingOutgoing, ok: store.PeeringPendingOutgoing},
		{name: "approved", state: store.PeeringApproved, ok: store.PeeringApproved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := approvedState(tc.state, tc.err)
			if tc.ok != 0 {
				if err != nil || got != tc.ok {
					t.Fatalf("approvedState = %v, %v; want %v", got, err, tc.ok)
				}
				return
			}
			if connect.CodeOf(err) != tc.want {
				t.Fatalf("approvedState error = %v, want code %v", err, tc.want)
			}
		})
	}
}
