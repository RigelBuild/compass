package gateway

import (
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// maxControlAckBytes bounds one frame's encoded ControlAck. Packed repeated uint64
// decodes a one-byte varint into eight bytes, so the 16MiB read cap alone admits a
// ~128MiB ack. An honest ack stays far below this.
const maxControlAckBytes = 1 << 20

// Proto field numbers the pre-scan walks: the request's AgentFrame and its ack.
const (
	requestFrameField    protowire.Number = 1
	agentFrameAckField   protowire.Number = 5
	codecNameProtoBinary                  = "proto"
)

var errControlAckTooLarge = errors.New("control ack exceeds size bound")

// ackBoundCodec is the binary proto codec with an encoded-size check on ControlAck,
// run before decode so an oversized ack never allocates its repeated fields.
type ackBoundCodec struct{}

var _ connect.Codec = ackBoundCodec{}

func (ackBoundCodec) Name() string { return codecNameProtoBinary }

func (ackBoundCodec) Marshal(message any) ([]byte, error) {
	m, ok := message.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("marshal %T: not a proto message", message)
	}
	return proto.Marshal(m)
}

func (ackBoundCodec) MarshalAppend(dst []byte, message any) ([]byte, error) {
	m, ok := message.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("marshal %T: not a proto message", message)
	}
	return proto.MarshalOptions{}.MarshalAppend(dst, m)
}

func (ackBoundCodec) Unmarshal(data []byte, message any) error {
	m, ok := message.(proto.Message)
	if !ok {
		return fmt.Errorf("unmarshal into %T: not a proto message", message)
	}
	switch m.(type) {
	case *compassv1internal.PublishFrameRequest, *compassv1internal.PostConversationFrameRequest:
		if n := controlAckBytes(data); n > maxControlAckBytes {
			return fmt.Errorf("%w: %d bytes, limit %d", errControlAckTooLarge, n, maxControlAckBytes)
		}
	}
	if err := proto.Unmarshal(data, m); err != nil {
		return fmt.Errorf("unmarshal into %T: %w", message, err)
	}
	return nil
}

// controlAckBytes sums every encoded ControlAck in a frame request. Proto merges
// repeated occurrences of a message field, so a split ack counts in full. The walk
// allocates nothing; malformed input stops it and proto.Unmarshal then reports it.
func controlAckBytes(request []byte) int {
	total := 0
	forEachBytesField(request, requestFrameField, func(frame []byte) {
		forEachBytesField(frame, agentFrameAckField, func(ack []byte) {
			total += len(ack)
		})
	})
	return total
}

// forEachBytesField calls fn with the payload of every length-delimited occurrence
// of num in b, in wire order.
func forEachBytesField(b []byte, num protowire.Number, fn func([]byte)) {
	for len(b) > 0 {
		n, typ, tagLen := protowire.ConsumeTag(b)
		if tagLen < 0 {
			return
		}
		b = b[tagLen:]
		vLen := protowire.ConsumeFieldValue(n, typ, b)
		if vLen < 0 {
			return
		}
		if typ == protowire.BytesType && n == num {
			v, _ := protowire.ConsumeBytes(b) // length already validated by ConsumeFieldValue
			fn(v)
		}
		b = b[vLen:]
	}
}

// rejectJSONCodec replaces Connect's default JSON codecs. The agent speaks binary
// gRPC only, and protojson would decode an unbounded ack array past the pre-scan.
type rejectJSONCodec struct{ name string }

var errJSONUnsupported = errors.New("AgentGateway accepts binary proto only")

func (c rejectJSONCodec) Name() string { return c.name }

func (rejectJSONCodec) Marshal(message any) ([]byte, error) {
	m, ok := message.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("marshal %T: not a proto message", message)
	}
	return protojson.Marshal(m)
}

func (rejectJSONCodec) Unmarshal([]byte, any) error { return errJSONUnsupported }

// agentGatewayHandlerOptions is the option set every AgentGateway mount uses.
func agentGatewayHandlerOptions() []connect.HandlerOption {
	return []connect.HandlerOption{
		connect.WithReadMaxBytes(maxAgentMessageBytes),
		connect.WithCodec(ackBoundCodec{}),
		connect.WithCodec(rejectJSONCodec{name: "json"}),
		connect.WithCodec(rejectJSONCodec{name: "json; charset=utf-8"}),
	}
}
