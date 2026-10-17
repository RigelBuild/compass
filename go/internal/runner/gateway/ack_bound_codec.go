package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectproto"
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

var (
	_                connect.Codec = ackBoundCodec{}
	binaryProtoCodec               = connectproto.NewBinaryCodec()
)

func (ackBoundCodec) Name() string { return codecNameProtoBinary }

func (ackBoundCodec) MarshalWrite(ctx context.Context, dst io.Writer, message any) error {
	return binaryProtoCodec.MarshalWrite(ctx, dst, message)
}

func (ackBoundCodec) UnmarshalRead(_ context.Context, src io.Reader, message any) error {
	data, err := readAgentMessage(src)
	if err != nil {
		return err
	}
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

func readAgentMessage(src io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(src, int64(maxAgentMessageBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxAgentMessageBytes {
		return nil, connect.Errorf(
			connect.CodeResourceExhausted,
			"message is larger than configured max %d",
			maxAgentMessageBytes,
		)
	}
	return data, nil
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

// frameJSONCodec is Connect's protojson codec, except that it refuses the two
// AgentFrame-carrying requests: protojson would decode an unbounded ack array past
// the binary pre-scan, and the agent sends frames as binary gRPC only.
type frameJSONCodec struct {
	name  string
	codec *connectproto.JSONCodec
}

var errJSONUnsupported = errors.New("AgentGateway frame requests accept binary proto only")

func newFrameJSONCodec(name string) frameJSONCodec {
	return frameJSONCodec{name: name, codec: connectproto.NewJSONCodec()}
}

func (c frameJSONCodec) Name() string { return c.name }

func (c frameJSONCodec) MarshalWrite(ctx context.Context, dst io.Writer, message any) error {
	return c.codec.MarshalWrite(ctx, dst, message)
}

func (c frameJSONCodec) UnmarshalRead(_ context.Context, src io.Reader, message any) error {
	data, err := readAgentMessage(src)
	if err != nil {
		return err
	}
	m, ok := message.(proto.Message)
	if !ok {
		return fmt.Errorf("unmarshal into %T: not a proto message", message)
	}
	switch m.(type) {
	case *compassv1internal.PublishFrameRequest, *compassv1internal.PostConversationFrameRequest:
		return errJSONUnsupported
	}
	if len(data) == 0 {
		return errors.New("zero-length payload is not a valid JSON object")
	}
	if err := c.codec.UnmarshalOptions.Unmarshal(data, m); err != nil {
		return fmt.Errorf("unmarshal into %T: %w", message, err)
	}
	return nil
}

// agentGatewayHandlerOptions applies the read cap and bounded codecs to each
// AgentGateway method.
func agentGatewayHandlerOptions() []connecthttp.Option {
	return []connecthttp.Option{
		connecthttp.WithReadMaxBytes(maxAgentMessageBytes),
		connecthttp.WithCodecs(
			ackBoundCodec{},
			newFrameJSONCodec(connect.CodecNameJSON),
			newFrameJSONCodec(connect.CodecNameJSON+"; charset=utf-8"),
		),
	}
}
