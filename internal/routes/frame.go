package routes

import (
	"fmt"
	"time"

	"github.com/streasure/protocol/commonstruct"
	protoGw "github.com/streasure/protocol/gateway"
	"google.golang.org/protobuf/proto"
)

// DecodeClientMessage 解码公共MessageFrame协议数据，提取业务protobuf负载。
// body是业务protobuf载荷，StreamData仅作为后端信封。
func DecodeClientMessage(data []byte) (*protoGw.StreamData, bool) {
	cmd, seqID, body, ok := ExtractMessageFrame(data)
	if !ok {
		return nil, false
	}
	return &protoGw.StreamData{
		Cmd:   cmd,
		SeqId: seqID,
		Data:  append([]byte(nil), body...),
	}, true
}

// MarshalClientMessage 将StreamData序列化为公共MessageFrame信封格式。
func MarshalClientMessage(msg *protoGw.StreamData) ([]byte, error) {
	if msg == nil {
		return nil, fmt.Errorf("nil message")
	}
	return proto.Marshal(&protoGw.MessageFrame{Cmd: msg.Cmd, SeqId: msg.SeqId, Body: msg.Data})
}

// MarshalClientError 将错误响应序列化为MessageFrame格式的字节数据。
func MarshalClientError(errMsg *commonstruct.ErrorResponse) []byte {
	if errMsg == nil {
		return nil
	}
	body, err := proto.Marshal(errMsg)
	if err != nil {
		return nil
	}
	framed, err := proto.Marshal(&protoGw.MessageFrame{
		Cmd:  CmdError,
		Body: body,
	})
	if err != nil {
		return nil
	}
	return framed
}

// NewErrorResponse 构造标准错误响应。
func NewErrorResponse(route, message, details, data string) *commonstruct.ErrorResponse {
	return &commonstruct.ErrorResponse{
		Route: route,
		Error: &commonstruct.ErrorData{
			Message: message,
			Code:    details,
			Details: data,
		},
		Timestamp: time.Now().UnixMilli(),
	}
}
