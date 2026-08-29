package gateway

import "github.com/streasure/sgate/obs"

// StartPProfServer 启动 pprof HTTP 服务（默认 :6060）
func StartPProfServer(addr string) { obs.StartPProfServer(addr) }

// StopPProfServer 停止 pprof server
func StopPProfServer() { obs.StopPProfServer() }
