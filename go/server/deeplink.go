//go:build unix

// The Linear Agent responder's "Open in Compass" deep-link builder (RIG-2717
// T5, design Part 2 / Part 6, OQ-4). The responder emits one externalUrls deep
// link on a Linear `created` session, targeting the resolved Manager's HOME
// CHANNEL (not the specific topic — Matt ruled OQ-4: link to the home channel
// and let the human navigate to the thread). The link's base is the
// per-deployment public URL (ServeConfig.PublicURL); the path is the UI's
// channel hash route (apps/ui/src/routes.tsx:40, `/channel/:channelId` under
// the HashRouter, so the on-the-wire form is `<base>/#/channel/<id>`).
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

// deepLinkFor builds the "Open in Compass" URL to a Manager's home channel from
// the per-deployment public base (design T5 / OQ-4). The UI is a HashRouter, so
// the channel surface lives at the `/#/channel/<id>` fragment route
// (apps/ui/src/routes.tsx:40); the channelID is path-escaped so an id with URL
// metacharacters cannot break out of the fragment. A trailing slash on the base
// is trimmed so `https://host/` and `https://host` yield the same link. An empty
// base yields a base-less (relative) fragment — callers gate on requirePublicURL
// at boot so this never happens for a Linear-webhook-consuming deploy.
func deepLinkFor(base, channelID string) string {
	return strings.TrimRight(base, "/") + "/#/channel/" + url.PathEscape(channelID)
}
