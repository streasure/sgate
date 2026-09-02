package obs

import (
	"net/http"
	"net/http/pprof"
	"sync"

	"github.com/streasure/util/tlog"
)

// pprofServer 全局 pprof 服务器（独立 goroutine，独立 listener）
// 暴露 /debug/pprof/{goroutine,heap,profile,trace,...}
// 用于：
//   - 监控 Goroutine 数量（防止泄漏 / 调度器过载）
//   - heap profile 分析内存泄漏
//   - goroutine profile 排查 schedlatency
//   - CPU profile 排查热点函数
var (
	pprofOnce   sync.Once
	pprofServer *http.Server
)

// StartPProfServer 启动 pprof HTTP 服务（默认 :6060）
// 多次调用幂等
func StartPProfServer(addr string) {
	if addr == "" {
		addr = ":6060"
	}
	pprofOnce.Do(func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/debug/pprof", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/debug/pprof/", http.StatusMovedPermanently)
		})
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		for _, name := range []string{"allocs", "block", "goroutine", "heap", "mutex", "threadcreate"} {
			mux.Handle("/debug/pprof/"+name, pprof.Handler(name))
		}
		pprofServer = &http.Server{Addr: addr, Handler: mux}
		go func() {
			tlog.Info("pprof server started", "addr", addr)
			if err := pprofServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				tlog.Warn("pprof server stopped", "error", err)
			}
		}()
	})
}

// StopPProfServer 停止 pprof server
func StopPProfServer() {
	if pprofServer != nil {
		pprofServer.Close()
	}
}
