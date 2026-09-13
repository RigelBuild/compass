// The account kind discriminant at the store <-> proto edge (mapping.go
// accountToWire): a store Account is one of User / Agent / System and the wire
// oneof must reflect it. The T2 arm: a System account must emit Account_System,
// never an unset kind (which the UI renders as a plain user). Pure, default lane.

package comms

import (
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

func TestAccountToWireEmitsKindPerSubtype(t *testing.T) {
	tests := []struct {
		name string
		acc  store.Account
		want func(*compassv1.Account) bool
	}{
		{
			name: "user",
			acc:  store.Account{ID: "u1", Handle: "matt", User: &store.UserAccount{Role: store.UserRoleMember}},
			want: func(a *compassv1.Account) bool { _, ok := a.GetKind().(*compassv1.Account_User); return ok },
		},
		{
			name: "agent",
			acc:  store.Account{ID: "a1", Handle: "cook", Agent: &store.AgentAccount{OwnerUserID: "u1"}},
			want: func(a *compassv1.Account) bool { _, ok := a.GetKind().(*compassv1.Account_Agent); return ok },
		},
		{
			name: "system",
			acc:  store.Account{ID: "s1", Handle: store.SystemAccountHandle, System: &store.SystemAccount{}},
			want: func(a *compassv1.Account) bool { _, ok := a.GetKind().(*compassv1.Account_System); return ok },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := accountToWire(tt.acc)
			if out.GetId() != string(tt.acc.ID) || out.GetHandle() != tt.acc.Handle {
				t.Fatalf("scalar fields not carried: got id=%q handle=%q", out.GetId(), out.GetHandle())
			}
			if !tt.want(out) {
				t.Fatalf("wire kind = %T, want the %s oneof case", out.GetKind(), tt.name)
			}
		})
	}
}
