// The channel-kind discriminant at the store <-> proto edge (mapping.go
// channelKindToWire / channelKindFromWire). Pure, no database, default lane.

// GROUP_DM retirement (RIG-2962 T1): the kind is deprecated in place, never
// PRODUCED. No mapping arm may mint a GROUP_DM; the number stays reserved, but a
// legacy or hostile GROUP_DM input collapses to a plain channel.

package comms

import (
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

func TestChannelKindToWireNeverProducesGroupDM(t *testing.T) {
	tests := []struct {
		name string
		kind store.ChannelKind
		want compassv1.ChannelKind
	}{
		{"channel", store.ChannelKindChannel, compassv1.ChannelKind_CHANNEL_KIND_CHANNEL},
		{"dm", store.ChannelKindDM, compassv1.ChannelKind_CHANNEL_KIND_DM},
		{"retired group_dm collapses to channel", store.ChannelKindGroupDM, compassv1.ChannelKind_CHANNEL_KIND_CHANNEL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := channelKindToWire(tt.kind)
			if got != tt.want {
				t.Fatalf("channelKindToWire(%v) = %v, want %v", tt.kind, got, tt.want)
			}
			if got == compassv1.ChannelKind_CHANNEL_KIND_GROUP_DM { //nolint:staticcheck // SA1019: deliberately references the retired GROUP_DM to assert it is never produced.
				t.Fatalf("channelKindToWire produced retired GROUP_DM for %v", tt.kind)
			}
		})
	}
}

func TestChannelKindFromWireNeverProducesGroupDM(t *testing.T) {
	tests := []struct {
		name string
		kind compassv1.ChannelKind
		want store.ChannelKind
	}{
		{"channel", compassv1.ChannelKind_CHANNEL_KIND_CHANNEL, store.ChannelKindChannel},
		{"dm", compassv1.ChannelKind_CHANNEL_KIND_DM, store.ChannelKindDM},
		{"retired group_dm collapses to channel", compassv1.ChannelKind_CHANNEL_KIND_GROUP_DM, store.ChannelKindChannel}, //nolint:staticcheck // SA1019: deliberately exercises the retired GROUP_DM input path.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := channelKindFromWire(tt.kind)
			if got != tt.want {
				t.Fatalf("channelKindFromWire(%v) = %v, want %v", tt.kind, got, tt.want)
			}
			if got == store.ChannelKindGroupDM {
				t.Fatalf("channelKindFromWire produced retired GROUP_DM for %v", tt.kind)
			}
		})
	}
}
