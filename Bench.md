压测流程
websocket和tcp都需要压测。websocket优先级更高。这两个分开压测。
删除原有的bench和logic的设计和相关代码文件配置。
严禁在bench和logic做无用的什么回包校验。还有不可能存在authfail，存在就是sgate那边的登录校验代码错误。或者在bench和logic做了不该做的数据处理。
其中客户端和sgate的收发消息体都为E:protocol定义的MessageFrame
而sgate和logic之间的交互消息全是E:protocol定义的StreamData

sgate和logic之间是建立的stream流式通信。所有的client消息全走这个一个流，最后再通过userUuid去区分（这一步不用实现，只需要保证stream的通信性能即可）

压测顺序：先WebSocket，再TCP。

## bench1：客户端→sgate→逻辑服 转发压测

只测试websocket和tcp这一条链路的单向通信。
其中bench只负责logingatereq正常即可，其他的所有协议一律忽视回包只要logingatereq跑通，其他的协议recv直接全部丢弃不处理，logic更直接，只负责与sgate建立通信，收到的所有消息全部丢弃。
看sgate在logingatereq完成之后纯粹的转发性能。
重新设计符合这个要求的bench1和logic1。tcp和websocket分开写bench1_tcp+logic1_tcp和bench1_ws+logic1_ws。压测出sgate在这个链路的tcp和websocket的实际性能。

bench1具体的流程细节
Client: proto.Marshal(MessageFrame) + TCP/websocket write
sgate: TCP/websocket read → proto.Unmarshal(MessageFrame) → pipeline全量检查(已认证则跳过) → StreamShard channel → proto.Marshal(StreamData) → gRPC stream.Send
Logic: gRPC stream.Recv() → proto.Unmarshal(StreamData) → 丢弃

## bench2：逻辑服→sgate→客户端 推送压测

首先建立需要测试的连接。保证这一条链路是通的。这时候不开始不压测。
随后重新设计logic2.根据连接的useruuid去创建不同的组，针对这些组做推送。
其实相当于logic2变成了压测工具，测试的是logic2->sgate->bench2的性能。其中bench2。对这些推送recv直接丢弃。看的是sgate的推送转发性能和推送的准确性。
重新设计符合这个要求的bench2和logic2，tcp和websocket分开写bench2_tcp+logic2_tcp和bench2_ws+logic2_ws。压测出推送这块的实际性能。

bench2具体的流程细节
Client: proto.Marshal(MessageFrame) + TCP/websocket write
sgate: TCP/websocket read → proto.Unmarshal(MessageFrame) → pipeline全量检查(已认证则跳过) → StreamShard channel → proto.Marshal(StreamData) → gRPC stream.Send
Logic: gRPC stream.Recv() → proto.Unmarshal(StreamData) → 正确解析loginreq建立useruuid的session通信库(可以在这边等一段时间再去走推送逻辑) → logic推送逻辑组建立 → 执行推送逻辑(主要逻辑所在) → proto.Marshal(StreamData) → gRPC stream.Send
sgate: gRPC stream.Recv() → proto.Unmarshal(StreamData) → StreamShard channel → TCP/websocket write
Client: TCP/websocket read → proto.Unmarshal(MessageFrame) → 丢弃

## 编译和运行

```powershell
go build -o sgate.exe .\cmd\gateway
go build -o bench\logic1_tcp\logic1_tcp.exe .\bench\logic1_tcp
go build -o bench\logic1_ws\logic1_ws.exe .\bench\logic1_ws
go build -o bench\bench1_tcp\bench1_tcp.exe .\bench\bench1_tcp
go build -o bench\bench1_ws\bench1_ws.exe .\bench\bench1_ws
go build -o bench\logic2_tcp\logic2_tcp.exe .\bench\logic2_tcp
go build -o bench\logic2_ws\logic2_ws.exe .\bench\logic2_ws
go build -o bench\bench2_tcp\bench2_tcp.exe .\bench\bench2_tcp
go build -o bench\bench2_ws\bench2_ws.exe .\bench\bench2_ws
```

## 已验证结果

### 最新：架构重构后（2026-09-24）✅ 性能增加

测试日期：2026-09-24。背景：`gateway→backend→connection` 分层重构、etcd 枚举 key、`loginValidation.enabled` 开关（压测恒为 false）之后。

条件：100 连接、10s、64B 载荷、12 推送协程、`shardCount=96`、`connGroupCount=4`、bench2 使用 `config_batch_off.yaml`（`batchPush=false`）、`expected-members=0`、登录仅 LoginGate（无 HTTP login / 无 token 校验）。

#### bench1 转发（client→sgate→logic）

| 协议 | 总转发量(10s) | 平均速率 | 失败 | droppedAuth |
| --- | ---: | ---: | ---: | ---: |
| WebSocket | 506.8 万 | **502,216/s** | 0 | 0 |
| TCP | 575.9 万 | **563,237/s** | 0 | 0 |

#### bench2 推送（logic→sgate→client，batchPush=false）

| 协议 | 总接收量(10s) | 平均接收速率 | 失败 | droppedAuth |
| --- | ---: | ---: | ---: | ---: |
| WebSocket | 2,360.3 万 | **2,353,552/s** | 0 | 0 |
| TCP | 1,258.1 万 | **1,249,510/s** | 0 | 0 |

原始数据：`bench/latest_results.json`；bench2 日志：`logs/bench2_ws.log`、`logs/bench2_tcp.log`。

#### 与历史对比（是否变快）

| 场景 | 旧（09-13/14） | 新（09-24） | 变化 |
| --- | ---: | ---: | --- |
| bench1 WS 平均 | 487K/s | 502K/s | **+3.1% ↑** |
| bench1 TCP 平均 | 533K/s | 563K/s | **+5.6% ↑** |
| bench2 WS（batch=false） | 442K/s | 2,354K/s | **+433% ↑** |
| bench2 TCP（batch=false） | 421K/s | 1,250K/s | **+197% ↑** |
| bench2 WS（batch=true 旧最优） | 816K/s | 2,354K/s（本轮 batch 关） | **+188% ↑** |
| bench2 TCP（batch=true 旧最优） | 853K/s | 1,250K/s（本轮 batch 关） | **+47% ↑** |

**结论：重构后性能是增加的，无回退。** bench1 平均吞吐小幅上升；bench2 在同为 `batchPush=false` + 100 连接条件下提升约 2～5 倍。

注意：历史另有 1000 连接 + `connGroupCount` 口径（TCP 69.4 万 / WS 128.9 万），连接数不同，不能与本轮 100 连接直接横比。

### 历史：多 TCP 连接优化后（2026-09-14）

测试日期：2026-09-14。验证 standalone 模式下向 etcd 注册 JSON 格式连接信息后性能无回退。

| 协议 | 总转发量(10s) | 峰值速率 | 平均速率 |
| --- | ---: | ---: | ---: |
| TCP | 844 万 | 835K/s | 533K/s |
| WebSocket | 941 万 | 920K/s | 487K/s |

### 历史：batchPush 与 1000 连接口径（2026-09-13）

测试日期：2026-09-13。条件：100 连接、64 字节推送载荷、12 个推送工作协程、持续 10 秒、`push-interval=0`、96 个流分片。

#### bench1 转发

| 协议 | 总转发量(10s) | 峰值速率 |
| --- | ---: | ---: |
| WebSocket | 843 万 | 840K/s |
| TCP | 770 万 | 767K/s |

#### bench2 推送（100 连接）

| 模式 | TCP | WebSocket |
| --- | ---: | ---: |
| batchPush=false | 421K/s | 442K/s |
| batchPush=true | 853K/s | 816K/s |
| 提升 | 约 102% | 约 85% |

#### bench2 推送（1000 连接 + connGroupCount=4）

| 指标 | 改前（单 TCP） | 改后（4 TCP） | 提升 |
| --- | ---: | ---: | ---: |
| bench2_tcp | 39.3 万/s | 69.4 万/s | +76% |
| bench2_ws | 72.7 万/s | 128.9 万/s | +77% |

etcd 注册地址格式示例：
```json
{"ip":"10.5.20.7","grpc":50051,"tcp":"10.5.20.7:48080","websocket":"10.5.20.7:48081"}
```

2026-09-24 起 etcd key 改为 protocol 枚举：`/services/{belong}/{ServerTypeName}:{zone}/{instanceId}`（如 `SGATE` / `LOGINSERVER` / `LOGICSERVER`）。

