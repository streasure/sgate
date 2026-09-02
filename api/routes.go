// Package api defines gateway-level constants and error codes.
// Route constants are defined in protobuf/routes.go and re-exported here.
package api

import "github.com/streasure/protocol/sgate"

// Re-export route constants for convenience.
const (
	RouteHandshake         = sgate.RouteHandshake
	RouteHandshakeResponse = sgate.RouteHandshakeResponse
	RouteLogin             = sgate.RouteLogin
	RouteError             = sgate.RouteError
	RoutePing              = sgate.RoutePing
	RoutePong              = sgate.RoutePong
	RouteBatch             = sgate.RouteBatch

	RouteServerKick        = sgate.RouteServerKick
	RouteServerJoinGroup   = sgate.RouteServerJoinGroup
	RouteServerLeaveGroup  = sgate.RouteServerLeaveGroup
	RouteServerBroadcast   = sgate.RouteServerBroadcast
	RouteServerSendToUser  = sgate.RouteServerSendToUser
	RouteServerSendToGroup = sgate.RouteServerSendToGroup
)
