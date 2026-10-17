//go:build unix

package gateway

import (
	"errors"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestRelayedErrorPreservesRemoteError(t *testing.T) {
	detail, err := connectproto.NewErrorDetail(&emptypb.Empty{})
	if err != nil {
		t.Fatalf("new error detail: %v", err)
	}
	cause := errors.New("underlying transport cause")
	remote := connect.NewError(connect.CodeUnavailable, "server unreachable").WithDetail(detail).WithCause(cause).WithRemote()

	got := relayedError(remote)
	gotConnect, ok := errors.AsType[*connect.Error](got)
	if !ok {
		t.Fatalf("relayed error type = %T, want *connect.Error", got)
	}
	if gotConnect.IsRemote() {
		t.Fatal("relayed error remains remote")
	}
	if gotConnect.Code() != remote.Code() || gotConnect.Message() != remote.Message() {
		t.Fatalf("relayed error = (%v, %q), want (%v, %q)", gotConnect.Code(), gotConnect.Message(), remote.Code(), remote.Message())
	}
	gotDetails := gotConnect.Details()
	if len(gotDetails) != 1 || gotDetails[0].Type != detail.Type || string(gotDetails[0].Value) != string(detail.Value) {
		t.Fatalf("relayed details = %v, want original detail %v", gotDetails, detail)
	}
	if !errors.Is(got, cause) {
		t.Fatalf("relayed error %v does not preserve cause %v", got, cause)
	}
}

func TestRelayedErrorLeavesLocalErrorUnchanged(t *testing.T) {
	local := connect.NewError(connect.CodeUnavailable, "local failure")
	if got := relayedError(local); got != local {
		t.Fatalf("relayedError(local) = %v, want original error", got)
	}
}
