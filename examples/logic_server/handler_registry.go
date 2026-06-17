package main

import "github.com/streasure/sgate/logic"

var dispatcherBuilders []func() *logic.Dispatcher

var routeBuilders []func(svc *logic.Service)

func RegisterDispatcherBuilder(fn func() *logic.Dispatcher) {
	dispatcherBuilders = append(dispatcherBuilders, fn)
}

func RegisterRouteBuilder(fn func(svc *logic.Service)) {
	routeBuilders = append(routeBuilders, fn)
}

func ApplyAllHandlers(svc *logic.Service) {
	for _, fn := range routeBuilders {
		fn(svc)
	}
	for _, fn := range dispatcherBuilders {
		svc.RegisterDispatcher(fn())
	}
}
