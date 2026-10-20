package forge

import (
	"regexp"
	"strings"
)

const writeMarkerPrefix = "<!-- compass:write "

// writeMarkerRe matches any write marker, including a forged or malformed one.
var writeMarkerRe = regexp.MustCompile(`<!-- compass:write [^>]*-->`)

func hasWriteMarker(body, intentID string) bool {
	return strings.Contains(body, writeMarkerPrefix+intentID+" -->")
}

// WithWriteMarker appends the intent's marker after body, as mirror.WithMarker
// ends a mirror post.
func WithWriteMarker(body, intentID string) string {
	return strings.TrimRight(body, "\n") + "\n\n" + writeMarkerPrefix + intentID + " -->"
}

// StripWriteMarkers removes every write marker from agent text, so only the
// Server's own marker can identify a create.
func StripWriteMarkers(body string) string {
	return writeMarkerRe.ReplaceAllString(body, "")
}
