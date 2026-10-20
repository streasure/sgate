# sgate 待修清单

来源：2026-10-20 全仓实现审查（loginserver 侧 11 项已同期修复，见
`loginserver/docs/ban-convergence-design.md` §11）。按严重程度排序；
P0 会出安全/可用性问题，P1 数据错/竞态/泄漏，P2 可维护性。
修复后请跑门禁：`gofmt -l .` → `go build ./...` → `go vet ./...` →
`staticcheck ./...` → `go test ./... -count=1`。

## P0

### 1. 安全组件无法热启/热停（静默 fail-open / fail-stale）
- 位置：`internal/gateway/gateway.go` handleConfigUpdate（约 568-636 行）+
  `internal/component/security_component.go`（53-96 行）
- 问题：WAF/JWT/限流/熔断/降级只在启动时按配置创建；热更全部带
  `if g.xxx != nil` 前缀。配置中心把 `jwtAuth.enabled`/`rateLimit.enabled`/
  `waf.enabled`/`security.enabled` 从 false 改 true，Validate 通过、无报错，
  运行期完全不生效（JWT 等于被绕过）；改 false 则旧阈值继续生效（fail-stale），
  与同函数白名单"关闭即清空"（gateway.go:593-598）自相矛盾。
- 方向：组件按需懒创建 + 显式 disable 路径；或启动时强制要求相关配置不可热切，
  Validate 拒绝并告警。

### 2. maxFrameSize 与缓冲尺寸无交叉校验（超限静默断连）
- 位置：`internal/gateway/handlers.go:236,253`（return false 断连、零日志）、
  `internal/config/config.go:413-425`、`internal/config/defaults.go:73-75`
- 问题：两者零值都补 4MiB，但 feedFrames 要求缓冲 ≥ 4+frameLen：
  恰好 4MiB 的合法帧必被掐断；README 建议百万连接设 64KiB 缓冲时，
  >64KiB 的合法帧同样静默断连。线上表现为"随机掉线查无原因"。
- 方向：Validate 校验 `frameBuf >= maxFrameSize + 4`；超限路径加日志。

## P1

### 3. 离线通知先 Claim 再异步发且吞错（下线通知永久丢失）
- 位置：`internal/gateway/login.go:296-318`（298 Claim、315 goroutine、316 `_ =`）
- 问题：`ClaimOfflineNotify` 发送前置位，一旦发送失败（drop 策略队列满/
  ErrNotConnected），OnClose 再触发也 Claim 失败被跳过——通知既失败又不可重试，
  逻辑服留幽灵会话。
- 方向：发送成功后再置幂等标记，或失败时回滚 Claim。

### 4. 同账号多连接只索引一条，封禁可能漏踢
- 位置：`internal/connection/manager.go:242-250`（accountConnections.Store 覆盖写）+
  `internal/gateway/kick.go:81-93`
- 问题：`finishLoginGate` 只在 serverId:userId 相同时关旧连接（login.go:237），
  同账号登录不同逻辑服同时在线时索引被后者覆盖，`KickAccounts` 只踢中最新一条。
- 方向：accountConnections 改多连接集合（connID 列表）。

### 5. RemoteAddr 唯一裸字段（string 撕裂竞态）
- 位置：`internal/connection/connection.go:26`（裸字段）、`166-175`（写）、
  `154`（读）+ `manager.go:226,478-496`（idle-checker 并发读）
- 问题：结构体其余并发字段全用 atomic.Pointer，唯独 RemoteAddr 裸奔；
  idle-checker/CloseAllConnections 与事件循环并发读写，`-race` 必炸。
- 方向：改 atomic.Pointer[string] 或统一走已有原子封装。

### 6. QueuePolicyBlock 无超时（可卡死整个 gnet 事件循环）
- 位置：`internal/backend/client.go:329-339`（cond.Wait 仅靠 Close 唤醒）+
  调用点 `internal/gateway/pipeline.go:324`
- 问题：配置 block 策略后，逻辑服断连期 Enqueue 卡住整个事件循环（不止单连接）。
  默认 drop 才没触发；已有 timeout 策略却没在 block 上兜底。
- 方向：block 加等待超时（复用 timeout 语义）或同步路径禁用 block。

### 7. 降级 RecordResult 每条消息全局写锁（热路径串行化）
- 位置：`internal/gateway/pipeline.go:338-351` + `internal/traffic/degradation.go:103-104`
- 问题：单个 m.mu 保护全部 route 的 map，每条转发消息都 Lock；
  降级一开吞吐即崩（同仓 obs/latency.go 已做 16 分片，此处没有）。
- 方向：按 route 分片或 atomic 计数。

### 8. Logout ack 先 AsyncWrite 后立即 Close（ack 必丢）
- 位置：`internal/gateway/websocket.go:509-515`（对比 admin.go:135-141 的 SendThenClose）
- 问题：connection.go:281-284 已明确该模式会丢通知（Windows 无 outbound flush），
  踢人路径已改 SendThenClose，登出 ack 仍是原始写法。
- 方向：复用 connection.SendThenClose。

## P2

### 9. 三处裸 go 无 panic 保护
- 位置：`internal/security/ratelimit.go:166`、`internal/obs/tracing.go:65`、
  `internal/obs/otel.go:67`
- 问题：其余后台协程统一走 panicutil.Go/GoLoop/Recover，这三处遗漏；
  偶发 panic 直接崩网关而非记日志。
- 方向：改 `panicutil.Go`（otel.go:207 的热路径 EndSpan 已改，67 行是另一处）。

### 10. defaults.go 死常量与真实默认不一致
- 位置：`internal/config/defaults.go:31,35,48,75,165-166` vs
  `config.go:416-419,447-457,549-550`；gcPercent 注释 config.go:61
- 问题：DefaultGRPCWindowSize（16MB）与实际用的 DefaultGatewayGRPCWindowSize
  （512KB）差 32 倍等多处"改了不生效"；GcPercent 注释 100 实际 300。
- 方向：删死常量或让真实默认引用它们；修正注释。

### 11. MessageIntegrity 启用后每消息全局写锁 + Sprintf 分配
- 位置：`internal/gateway/integrity.go:17,63-70`（启用点 pipeline.go:257）
- 问题：protection.verifyInbound=true 时所有连接共享一个 map 的写锁，
  每消息一次 fmt.Sprintf 分配；声明 RWMutex 却从不用读锁。
- 方向：分片 + 预分配 msgID（atomic counter 替代 Sprintf）。

### 12. 发送队列丢最旧不归池、RequeueFront 整段重分配
- 位置：`internal/backend/client.go:379-383`（drop）、`409-412`（满时丢弃 +
  append 头插）、`453`（Flush 全量 RequeueFront）
- 问题：被丢弃的 *StreamData 未 PutStreamData（池无法复用）；
  queue[1:] 移窗后底层数组长期滞留已丢消息指针（最多 maxSize 条）；
  长队列 Flush 重试 O(n²) 分配。
- 方向：drop 时归还池；RequeueFront 改环形/双端结构或批量一次拷贝。
