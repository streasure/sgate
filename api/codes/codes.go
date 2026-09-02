package codes

import "errors"

var (
	// Gateway-level error codes (200000+)
	ErrServerInternal = errors.New("server internal error")
	ErrSessionNotFound = errors.New("session not found")
	ErrForceCloseConn  = errors.New("force close connection")
	ErrStreamBusy      = errors.New("server stream is busy")
	ErrServerBusy      = errors.New("server overload, try again later")

	// Protocol-level error codes
	ErrRateLimit      = errors.New("rate limit exceeded")
	ErrUnknownError   = errors.New("unknown error")
	ErrBackendNotFound = errors.New("backend service not found")
	ErrAuthFailed     = errors.New("authentication failed")
	ErrHandshakeFailed = errors.New("handshake failed")
)
