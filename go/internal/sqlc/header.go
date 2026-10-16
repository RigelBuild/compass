// Package sqlc provides helpers for sqlc-generated query headers.
package sqlc

import "strings"

const header = "-- name: "

// QueryName returns the name from a sqlc-generated query header.
func QueryName(query string) (string, bool) {
	rest, ok := strings.CutPrefix(query, header)
	if !ok {
		return "", false
	}
	name, _, _ := strings.Cut(rest, " ")
	return name, true
}
