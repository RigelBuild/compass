//go:build unix

package server

import (
	"errors"
	"testing"
)

// TestDeepLinkFor pins the "Open in Compass" builder (RIG-2717 T5, OQ-4): the
// URL targets the Manager's home channel at the UI's `/#/channel/<id>` hash
// route, a trailing slash on the base collapses so `host/` and `host` agree, and
// a channel id carrying URL metacharacters is path-escaped so it cannot break
// out of the fragment.
func TestDeepLinkFor(t *testing.T) {
	tests := []struct {
		name      string
		base      string
		channelID string
		want      string
	}{
		{
			name:      "managed base and channel",
			base:      "https://compass.rigel.build",
			channelID: "ch-svc-compass",
			want:      "https://compass.rigel.build/#/channel/ch-svc-compass",
		},
		{
			name:      "trailing slash on base is collapsed",
			base:      "https://compass.rigel.build/",
			channelID: "ch-svc-compass",
			want:      "https://compass.rigel.build/#/channel/ch-svc-compass",
		},
		{
			name:      "tailnet deploy base with port",
			base:      "https://host.example.ts.net:8443",
			channelID: "ch-home-mgr",
			want:      "https://host.example.ts.net:8443/#/channel/ch-home-mgr",
		},
		{
			name:      "channel id with metacharacters is path-escaped",
			base:      "https://compass.rigel.build",
			channelID: "ch a/b?c",
			want:      "https://compass.rigel.build/#/channel/ch%20a%2Fb%3Fc",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := deepLinkFor(tc.base, tc.channelID); got != tc.want {
				t.Errorf("deepLinkFor(%q, %q) = %q, want %q", tc.base, tc.channelID, got, tc.want)
			}
		})
	}
}

// TestDeepLinkRequirePublicURL pins the boot guard: an empty (or whitespace)
// base is a legible boot rejection, not a silently-broken link, because a deploy
// that consumes Linear webhooks needs a reachable public URL (design T5). A
// non-empty base passes.
func TestDeepLinkRequirePublicURL(t *testing.T) {
	t.Run("empty base is rejected at boot", func(t *testing.T) {
		if err := requirePublicURL(""); !errors.Is(err, errNoPublicURL) {
			t.Errorf("requirePublicURL(%q) = %v, want errNoPublicURL", "", err)
		}
	})
	t.Run("whitespace-only base is rejected at boot", func(t *testing.T) {
		if err := requirePublicURL("   "); !errors.Is(err, errNoPublicURL) {
			t.Errorf("requirePublicURL(%q) = %v, want errNoPublicURL", "   ", err)
		}
	})
	t.Run("non-empty base passes", func(t *testing.T) {
		if err := requirePublicURL("https://compass.rigel.build"); err != nil {
			t.Errorf("requirePublicURL(valid) = %v, want nil", err)
		}
	})
}
