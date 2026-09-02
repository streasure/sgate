package gateway

import (
	"fmt"

	"github.com/streasure/protocol/commonstruct"
	"github.com/streasure/protocol/sgate"
	"google.golang.org/protobuf/proto"
)

// decodeClientMessage accepts the public MessageFrame protocol and the
// previous internal envelope during migration. MessageFrame body is passed
// unchanged in Data so logic can decode it according to Cmd.
func decodeClientMessage(data []byte) (*commonstruct.Message, bool) {
	cmd, seqID, body, ok := sgate.ExtractMessageFrame(data)
	if !ok {
		msg := new(commonstruct.Message)
		if err := proto.Unmarshal(data, msg); err != nil || msg.Route == "" {
			return nil, false
		}
		return msg, true
	}

	msg := &commonstruct.Message{
		Route:    sgate.RouteForCmd(cmd),
		Cmd:      cmd,
		Sequence: seqID,
		Data:     append([]byte(nil), body...),
	}
	// Generic clients may use a Message as the frame body. Preserve its
	// route/payload fields while retaining the frame command and sequence.
	inner := new(commonstruct.Message)
	if err := proto.Unmarshal(body, inner); err == nil && inner.Route != "" {
		inner.Cmd = cmd
		inner.Sequence = seqID
		return inner, true
	}
	return msg, true
}

// marshalClientMessage emits the public MessageFrame envelope. Internal
// control messages without a body use their serialized Message as body.
func marshalClientMessage(msg *commonstruct.Message) ([]byte, error) {
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
		cmd = sgate.CmdForRoute(msg.Route)
	}
	return proto.Marshal(&commonstruct.MessageFrame{Cmd: cmd, SeqId: msg.Sequence, Body: body})
}

// marshalClientBytes converts an internal Message payload to the public frame
// format. Raw non-Message payloads are left unchanged for transport helpers
// that already provide a complete frame.
func marshalClientBytes(data []byte) []byte {
	msg := new(commonstruct.Message)
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
	framed, err := proto.Marshal(&commonstruct.MessageFrame{
		Cmd:  sgate.CmdError,
		Body: body,
	})
	if err != nil {
		return nil
	}
	return framed
}
