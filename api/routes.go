// Package api defines gateway-level constants and error codes.
// Route constants are defined in protobuf/routes.go and re-exported here.
package api

import "github.com/streasure/protocol/gateway"

// Re-export route constants for convenience.
const (
	RouteHandshake         = gateway.RouteHandshake
	RouteHandshakeResponse = gateway.RouteHandshakeResponse
	RouteLogin             = gateway.RouteLogin
	RouteError             = gateway.RouteError
	RoutePing              = gateway.RoutePing
	RoutePong              = gateway.RoutePong
	RouteBatch             = gateway.RouteBatch

	RouteServerKick        = gateway.RouteServerKick
	RouteServerJoinGroup   = gateway.RouteServerJoinGroup
	RouteServerLeaveGroup  = gateway.RouteServerLeaveGroup
	RouteServerBroadcast   = gateway.RouteServerBroadcast
	RouteServerSendToUser  = gateway.RouteServerSendToUser
	RouteServerSendToGroup = gateway.RouteServerSendToGroup
)
