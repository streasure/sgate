package codes

import "errors"

var (
	// 网关层错误码（200000 以上）。
	ErrServerInternal  = errors.New("server internal error")
	ErrSessionNotFound = errors.New("session not found")
	ErrForceCloseConn  = errors.New("force close connection")
	ErrStreamBusy      = errors.New("server stream is busy")
	ErrServerBusy      = errors.New("server overload, try again later")

	// 协议层错误码。
	ErrRateLimit       = errors.New("rate limit exceeded")
	ErrUnknownError    = errors.New("unknown error")
	ErrBackendNotFound = errors.New("backend service not found")
	ErrAuthFailed      = errors.New("authentication failed")
	ErrHandshakeFailed = errors.New("handshake failed")
)
