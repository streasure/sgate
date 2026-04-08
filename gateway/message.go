package gateway

import (
	"sync"

	"github.com/streasure/sgate/gateway/protobuf"
)

// 消息对象池
var (
	requestMessagePool = sync.Pool{
		New: func() interface{} {
			return &protobuf.Message{}
		},
	}

	responseMessagePool = sync.Pool{
		New: func() interface{} {
			return &protobuf.Message{}
		},
	}

	errorMessagePool = sync.Pool{
		New: func() interface{} {
			return &protobuf.ErrorResponse{}
		},
	}
)

// GetRequestMessage 从对象池获取请求消息
func GetRequestMessage() *protobuf.Message {
	return requestMessagePool.Get().(*protobuf.Message)
}

// PutRequestMessage 归还请求消息到对象池
func PutRequestMessage(msg *protobuf.Message) {
	msg.ConnectionId = ""
	msg.UserUuid = ""
	msg.Route = ""
	if msg.Payload != nil {
		for k := range msg.Payload {
			delete(msg.Payload, k)
		}
	}
	requestMessagePool.Put(msg)
}

// GetResponseMessage 从对象池获取响应消息
func GetResponseMessage() *protobuf.Message {
	return responseMessagePool.Get().(*protobuf.Message)
}

// PutResponseMessage 归还响应消息到对象池
func PutResponseMessage(msg *protobuf.Message) {
	msg.ConnectionId = ""
	msg.UserUuid = ""
	msg.Route = ""
	if msg.Payload != nil {
		for k := range msg.Payload {
			delete(msg.Payload, k)
		}
	}
	responseMessagePool.Put(msg)
}

// GetErrorMessage 从对象池获取错误消息
func GetErrorMessage() *protobuf.ErrorResponse {
	return errorMessagePool.Get().(*protobuf.ErrorResponse)
}

// PutErrorMessage 归还错误消息到对象池
func PutErrorMessage(msg *protobuf.ErrorResponse) {
	msg.Route = ""
	if msg.Data != nil {
		msg.Data.Message = ""
		msg.Data.Error = ""
		msg.Data.Data = ""
	}
	errorMessagePool.Put(msg)
}

// NewRequestMessage 创建请求消息
func NewRequestMessage(connectionID, userUUID, route string, payload interface{}) *protobuf.Message {
	msg := GetRequestMessage()
	msg.ConnectionId = connectionID
	msg.UserUuid = userUUID
	msg.Route = route
	if payloadMap, ok := payload.(map[string]string); ok {
		msg.Payload = payloadMap
	} else if payloadStr, ok := payload.(string); ok {
		msg.Payload = map[string]string{"data": payloadStr}
	} else {
		msg.Payload = map[string]string{}
	}
	return msg
}

// NewResponseMessage 创建响应消息
func NewResponseMessage(route string, data interface{}) *protobuf.Message {
	msg := GetResponseMessage()
	msg.Route = route
	if dataMap, ok := data.(map[string]string); ok {
		msg.Payload = dataMap
	} else if dataStr, ok := data.(string); ok {
		msg.Payload = map[string]string{"data": dataStr}
	} else {
		msg.Payload = map[string]string{}
	}
	return msg
}

// NewErrorMessage 创建错误消息
func NewErrorMessage(route, message, err, data string) *protobuf.ErrorResponse {
	msg := GetErrorMessage()
	msg.Route = route
	msg.Data = &protobuf.ErrorData{
		Message: message,
		Error:   err,
		Data:    data,
	}
	return msg
}
