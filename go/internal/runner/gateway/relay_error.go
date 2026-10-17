//go:build unix

package gateway

import (
	"errors"

	"connectrpc.com/connect/v2"
)

// relayedError re-authors a remote verdict before a handler returns it.
func relayedError(err error) error {
	upstream, ok := errors.AsType[*connect.Error](err)
	if !ok || upstream == nil || !upstream.IsRemote() {
		return err
	}

	relayed := connect.NewError(upstream.Code(), upstream.Message())
	for _, detail := range upstream.Details() {
		relayed = relayed.WithDetail(detail)
	}
	return relayed.WithCause(err)
}
