// Package api defines gateway-level constants and error codes.
// Route constants are defined in protobuf/routes.go and re-exported here.
package api

import "github.com/streasure/sgate/protobuf"

// Re-export route constants for convenience.
const (
	RouteHandshake          = protobuf.RouteHandshake
	RouteHandshakeResponse  = protobuf.RouteHandshakeResponse
	RouteLogin              = protobuf.RouteLogin
	RouteError              = protobuf.RouteError
	RoutePing               = protobuf.RoutePing
	RoutePong               = protobuf.RoutePong
	RouteBatch              = protobuf.RouteBatch

	RouteServerKick             = protobuf.RouteServerKick
	RouteServerJoinGroup        = protobuf.RouteServerJoinGroup
	RouteServerLeaveGroup       = protobuf.RouteServerLeaveGroup
	RouteServerBroadcast        = protobuf.RouteServerBroadcast
	RouteServerSendToUser       = protobuf.RouteServerSendToUser
	RouteServerSendToGroup      = protobuf.RouteServerSendToGroup
)
