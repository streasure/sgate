//go:build legacy

package gateway

import (
	"fmt"

	"github.com/streasure/protocol/commonstruct"
	protoGw "github.com/streasure/protocol/gateway"
	routes "github.com/streasure/sgate/gateway"
	"google.golang.org/protobuf/proto"
)

// decodeClientMessage accepts the public MessageFrame protocol. The body is
// the business protobuf payload; StreamData is only the backend envelope.
func decodeClientMessage(data []byte) (*protoGw.StreamData, bool) {
	cmd, seqID, body, ok := routes.ExtractMessageFrame(data)
	if !ok {
		return nil, false
	}
	return &protoGw.StreamData{
		Cmd:   cmd,
		SeqId: seqID,
		Data:  append([]byte(nil), body...),
	}, true
}

// marshalClientMessage emits the public MessageFrame envelope. Internal
// control messages without a body use their serialized Message as body.
func marshalClientMessage(msg *protoGw.StreamData) ([]byte, error) {
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
	return proto.Marshal(&protoGw.MessageFrame{Cmd: msg.Cmd, SeqId: msg.SeqId, Body: body})
}

// marshalClientBytes converts an internal Message payload to the public frame
// format. Raw non-Message payloads are left unchanged for transport helpers
// that already provide a complete frame.
func marshalClientBytes(data []byte) []byte {
	return data
}

func marshalClientError(errMsg *commonstruct.ErrorResponse) []byte {
	if errMsg == nil {
		return nil
	}
	body, err := proto.Marshal(errMsg)
	if err != nil {
		return nil
	}
	framed, err := proto.Marshal(&protoGw.MessageFrame{
		Cmd:  routes.CmdError,
		Body: body,
	})
	if err != nil {
		return nil
	}
	return framed
}
