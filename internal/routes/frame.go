package routes

import (
	"fmt"
	"sync"
	"time"

	"github.com/streasure/protocol/commonstruct"
	protoGw "github.com/streasure/protocol/gateway"
	"google.golang.org/protobuf/proto"
)

// decodePool 复用入站解码产出的 StreamData 信封（热路径：每消息一解码）。
var decodePool = sync.Pool{
	New: func() any {
		return &protoGw.StreamData{}
	},
}

// DecodeClientMessage 解码公共MessageFrame协议数据，提取业务protobuf负载。
// body是业务protobuf载荷，StreamData仅作为后端信封。
// 返回的消息由调用方持有，处理完成后必须调用 PutClientMessage 归还
// （login/logout 等异步持有场景除外——不归还，交给 GC）。
func DecodeClientMessage(data []byte) (*protoGw.StreamData, bool) {
	cmd, seqID, body, ok := ExtractMessageFrame(data)
	if !ok {
		return nil, false
	}
	msg := decodePool.Get().(*protoGw.StreamData)
	kept := msg.Data[:0]
	msg.Reset()
	msg.Data = kept
	msg.Cmd = cmd
	msg.SeqId = seqID
	msg.Data = append(msg.Data, body...)
	return msg, true
}

// PutClientMessage 归还 DecodeClientMessage 产出的消息，保留 Data 缓冲容量。
// 归还后禁止继续使用该消息（含其 Data 切片）。
func PutClientMessage(msg *protoGw.StreamData) {
	if msg == nil {
		return
	}
	kept := msg.Data[:0]
	msg.Reset()
	msg.Data = kept
	decodePool.Put(msg)
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

// NewErrorFrame 构造标准错误响应并序列化为 MessageFrame 字节（写回客户端前一步到位）。
func NewErrorFrame(route, message, details, data string) []byte {
	return MarshalClientError(NewErrorResponse(route, message, details, data))
}
