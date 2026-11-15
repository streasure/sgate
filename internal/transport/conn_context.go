package transport

import (
	"context"
	"encoding/binary"
	"sync"

	"github.com/streasure/util/tlog"
)

var connContextPool = sync.Pool{
	New: func() any {
		return &ConnContext{
			ConnectionID: "",
			FrameBuf:     nil,
		}
	},
}

func GetConnContext() *ConnContext {
	ctx := connContextPool.Get().(*ConnContext)
	ctx.ConnectionID = ""
	ctx.FrameBuf = nil
	ctx.frameOff = 0
	ctx.LogCtx = nil
	return ctx
}

func PutConnContext(ctx *ConnContext) {
	ctx.ConnectionID = ""
	ctx.FrameBuf = nil
	ctx.frameOff = 0
	ctx.LogCtx = nil
	connContextPool.Put(ctx)
}

type ConnContext struct {
	ConnectionID string
	// LogCtx 日志上下文（OnOpen 置为 gateway ctx；测试可为 nil，超限日志按需跳过）
	LogCtx context.Context
	// FrameBuf 为底层数组（跨消息复用），有效数据 = FrameBuf[frameOff:]。
	// frameOff 为已消费偏移：置 nil 会让下一次读事件重新分配（实测 ~6B/条），
	// 中段切片 FrameBuf[n:] 又会丢失数组头部容量；offset 方案两者皆免。
	FrameBuf []byte
	frameOff int
	// proxyHeaderParsed PROXY protocol 头已消费标记（每连接只解析一次首包）
	proxyHeaderParsed bool
}

// feedFrames 将新读取的数据并入帧缓冲（frameOff 偏移复用，见 ConnContext 注释），
// 依次回调每个完整帧。fn 返回 false（业务断连）或帧非法（长度 0 / 超 maxFrame /
// 缓冲超限）时返回 false，调用方应关闭连接；返回 true 表示正常（可能仍有残帧待后续读取）。
func (ctx *ConnContext) feedFrames(data []byte, maxFrameBuf, maxFrame int, fn func(frame []byte) bool) bool {
	// 已收未消费的逻辑长度；超限断连（防 FrameBuf 无界增长）
	if len(ctx.FrameBuf)-ctx.frameOff+len(data) > maxFrameBuf {
		if ctx.LogCtx != nil {
			tlog.Warn(ctx.LogCtx, "tcp frame buffer overflow, closing connectionID=%s pending=%d incoming=%d maxFrameBufSize=%d",
				ctx.ConnectionID, len(ctx.FrameBuf)-ctx.frameOff, len(data), maxFrameBuf)
		}
		ctx.FrameBuf = nil
		ctx.frameOff = 0
		return false
	}
	// 有已消费前缀时前移残留，回收底层数组头部容量（append 热路径零重分配）
	if ctx.frameOff > 0 {
		logical := len(ctx.FrameBuf) - ctx.frameOff
		copy(ctx.FrameBuf, ctx.FrameBuf[ctx.frameOff:])
		ctx.FrameBuf = ctx.FrameBuf[:logical]
		ctx.frameOff = 0
	}
	ctx.FrameBuf = append(ctx.FrameBuf, data...)

	// 使用正常协议路径处理每个完整帧。逻辑流携带每条消息的真实客户端命令；它不定义网关私有的批处理命令。
	for len(ctx.FrameBuf)-ctx.frameOff >= 4 {
		frameLen := binary.BigEndian.Uint32(ctx.FrameBuf[ctx.frameOff:])
		if frameLen == 0 || frameLen > uint32(maxFrame) {
			if ctx.LogCtx != nil {
				tlog.Warn(ctx.LogCtx, "tcp frame length invalid, closing connectionID=%s frameLen=%d maxFrameSize=%d",
					ctx.ConnectionID, frameLen, maxFrame)
			}
			ctx.FrameBuf = nil
			ctx.frameOff = 0
			return false
		}
		totalLen := 4 + int(frameLen)
		if len(ctx.FrameBuf)-ctx.frameOff < totalLen {
			// 帧未收全：等待下一次读事件（残留由下次调用前移）
			return true
		}

		frameData := ctx.FrameBuf[ctx.frameOff+4 : ctx.frameOff+totalLen]
		ctx.frameOff += totalLen

		if !fn(frameData) {
			return false
		}
	}
	// 全部帧消费完：保留底层数组容量（[:0] 而非 nil），下次读事件直接复用
	if ctx.frameOff == len(ctx.FrameBuf) {
		ctx.FrameBuf = ctx.FrameBuf[:0]
		ctx.frameOff = 0
	}
	return true
}
