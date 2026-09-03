package gateway

import (
	"fmt"

	"github.com/streasure/protocol/commonstruct"
	"github.com/streasure/protocol/gateway"
	"google.golang.org/protobuf/proto"
)

// decodeClientMessage accepts the public MessageFrame protocol. Clients wrap a
// serialized backend.StreamData as the MessageFrame body; the gateway extracts
// it to obtain Route, Payload and other fields for routing/handling.
func decodeClientMessage(data []byte) (*gateway.StreamData, bool) {
	cmd, seqID, body, ok := gateway.ExtractMessageFrame(data)
	if !ok {
		return nil, false
	}
	inner := new(gateway.StreamData)
	if err := proto.Unmarshal(body, inner); err == nil && inner.Route != "" {
		inner.Cmd = cmd
		inner.SeqId = seqID
		return inner, true
	}
	return &gateway.StreamData{
		Route: gateway.RouteForCmd(cmd),
		Cmd:   cmd,
		SeqId: seqID,
		Data:  append([]byte(nil), body...),
	}, true
}

// marshalClientMessage emits the public MessageFrame envelope. Internal
// control messages without a body use their serialized Message as body.
func marshalClientMessage(msg *gateway.StreamData) ([]byte, error) {
	if msg == nil {
		return nil, fmt.Errorf("nil message")
	}
	body := msg.Data
	if len(body) == 0 {
		var err error
		body, err = proto.Marshal(msg)
		if err != nil {
			return nil, err
		}
	}
	cmd := msg.Cmd
	if cmd == 0 {
		cmd = gateway.CmdForRoute(msg.Route)
	}
	return proto.Marshal(&gateway.MessageFrame{Cmd: cmd, SeqId: msg.SeqId, Body: body})
}

// marshalClientBytes converts an internal Message payload to the public frame
// format. Raw non-Message payloads are left unchanged for transport helpers
// that already provide a complete frame.
func marshalClientBytes(data []byte) []byte {
	msg := new(gateway.StreamData)
	if err := proto.Unmarshal(data, msg); err != nil || msg.Route == "" {
		return data
	}
	framed, err := marshalClientMessage(msg)
	if err != nil {
		return data
	}
	return framed
}

func marshalClientError(errMsg *commonstruct.ErrorResponse) []byte {
	if errMsg == nil {
		return nil
	}
	body, err := proto.Marshal(errMsg)
	if err != nil {
		return nil
	}
	framed, err := proto.Marshal(&gateway.MessageFrame{
		Cmd:  gateway.CmdError,
		Body: body,
	})
	if err != nil {
		return nil
	}
	return framed
}
