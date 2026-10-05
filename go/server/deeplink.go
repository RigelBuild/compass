//go:build unix

// The responder emits a stable /l/session/<id> link, which redirects at click time
// to the current Manager home channel built by deepLinkFor. Both use PublicURL.
package server

import (
	"errors"
	"net/url"
	"strings"
)

// errNoPublicURL is the boot rejection for an empty public base URL. A
// deployment that consumes Linear webhooks needs a reachable public URL to build
// the "Open in Compass" deep link (design T5); an empty base would silently emit
// a scheme-less relative link that opens nothing, so the responder assembly
// fails fast at boot rather than degrading to a broken link per event.
var errNoPublicURL = errors.New(
	"a public base URL is required to build Linear deep links: pass --public-url or set $COMPASS_PUBLIC_URL")

// requirePublicURL is the boot guard the responder assembly calls before wiring
// the Linear webhook path: it rejects an empty base with a legible error rather
// than letting deepLinkFor emit a base-less relative link at request time. There
// is no default base, so this fires whenever a deploy enables Linear webhooks
// without setting --public-url / $COMPASS_PUBLIC_URL.
func requirePublicURL(base string) error {
	if strings.TrimSpace(base) == "" {
		return errNoPublicURL
	}
	return nil
}

// deepLinkFor builds the UI's HashRouter channel link from the per-deployment
// public base. The channel ID is escaped and trailing slashes are trimmed.
func deepLinkFor(base, channelID string) string {
	return strings.TrimRight(base, "/") + "/#/channel/" + url.PathEscape(channelID)
}

// sessionLinkFor builds the stable click-time redirect URL from the Linear
// session ID; trimming and escaping match deepLinkFor's base and path handling.
func sessionLinkFor(base, linearSessionID string) string {
	return strings.TrimRight(base, "/") + "/l/session/" + url.PathEscape(linearSessionID)
}
