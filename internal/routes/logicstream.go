package routes

import (
	"context"
	"strconv"

	"google.golang.org/grpc/metadata"
)

// gRPC 流握手元数据键。
// 旧模式：sgate 主动拨入 logic，携带 MetaGatewayID。
// 新模式（flip）：logic 主动拨入 sgate，携带 MetaLogic* + 分片参数。
const (
	MetaGatewayID  = "sgate-gateway-id"  // sgate → logic（旧模式）
	MetaLogicID    = "sgate-logic-id"    // logic → sgate：逻辑服实例 ID
	MetaLogicZone  = "sgate-logic-zone"  // logic → sgate：逻辑服可用区
	MetaLogicAddr  = "sgate-logic-addr"  // logic → sgate：逻辑服对外地址（诊断）
	MetaShardIdx   = "sgate-shard-idx"   // logic → sgate：当前流分片序号
	MetaShardCount = "sgate-shard-count" // logic → sgate：分片总数
)

// LogicStreamMeta 逻辑服拨入网关的握手元数据。
type LogicStreamMeta struct {
	LogicID    string
	Zone       string
	Addr       string
	ShardIdx   int
	ShardCount int
}

// AppendLogicStreamMetadata 将握手元数据追加到出站上下文（logic 拨出流时调用）。
func AppendLogicStreamMetadata(ctx context.Context, m LogicStreamMeta) context.Context {
	return metadata.AppendToOutgoingContext(ctx,
		MetaLogicID, m.LogicID,
		MetaLogicZone, m.Zone,
		MetaLogicAddr, m.Addr,
		MetaShardIdx, strconv.Itoa(m.ShardIdx),
		MetaShardCount, strconv.Itoa(m.ShardCount),
	)
}

// LogicStreamMetadataFromContext 解析服务端入站握手元数据。
// 缺少 MetaLogicID 或分片参数非法时返回 false（调用方回落到旧版握手路径）。
func LogicStreamMetadataFromContext(ctx context.Context) (LogicStreamMeta, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return LogicStreamMeta{}, false
	}
	get := func(key string) string {
		if v := md.Get(key); len(v) > 0 {
			return v[0]
		}
		return ""
	}
	id := get(MetaLogicID)
	if id == "" {
		return LogicStreamMeta{}, false
	}
	idx, err := strconv.Atoi(get(MetaShardIdx))
	if err != nil || idx < 0 {
		return LogicStreamMeta{}, false
	}
	total, err := strconv.Atoi(get(MetaShardCount))
	if err != nil || total <= 0 || idx >= total {
		return LogicStreamMeta{}, false
	}
	return LogicStreamMeta{
		LogicID:    id,
		Zone:       get(MetaLogicZone),
		Addr:       get(MetaLogicAddr),
		ShardIdx:   idx,
		ShardCount: total,
	}, true
}
