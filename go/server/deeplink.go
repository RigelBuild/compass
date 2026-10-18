//go:build unix

// The responder emits a stable /l/session/<id> link, which redirects at click time
// to the current Manager home channel built by deepLinkFor. Both use PublicURL.
package server

import (
	"errors"
	"net/url"
	"strings"
)

// errNoPublicURL rejects webhook setup without a public base URL, which would
// otherwise make emitted deep links unusable.
var errNoPublicURL = errors.New(
	"a public base URL is required to build Linear deep links: pass --public-url or set $COMPASS_PUBLIC_URL")

// requirePublicURL rejects an empty base before Linear webhook setup; links
// have no default deployment URL.
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

// topicLinkFor builds the UI's HashRouter link to one topic in a channel.
func topicLinkFor(base, channelID, topicID string) string {
	if base == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/#/channel/" + url.PathEscape(channelID) + "/topic/" + url.PathEscape(topicID)
}

// sessionLinkFor builds the stable click-time redirect URL from the Linear
// session ID; trimming and escaping match deepLinkFor's base and path handling.
func sessionLinkFor(base, linearSessionID string) string {
	return strings.TrimRight(base, "/") + "/l/session/" + url.PathEscape(linearSessionID)
}
