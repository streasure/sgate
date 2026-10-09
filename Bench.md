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

### tools/client_logincheck（非压测工具）

`tools/client_logincheck` 是手动联调工具：调用 loginserver HTTP `/api/v1/login` 取 token 后走 LoginGate 验证登录链路。
**它不属于压测程序**（放在 `bench/` 下会违反 AGENTS.md「bench 禁止调用 /api/v1/login」），吞吐压测时不要编译/运行它。

## 已验证结果

原始数据：`bench/latest_results.json`（含逐秒明细与 `/stats` 快照）；bench2 明细见 `logs/sgate.log`（tlog 只写文件）。绝对数值随后台负载波动，同配置多轮约 ±10%。

### 测量条件（清单）

- 100 连接、单轮 10s、64B 载荷；登录仅 LoginGate（`loginValidation.enabled: false`，禁 HTTP login / token 校验）
- 网关 `shardCount=96`、`connGroupCount=4`、`sendChannelSize=65536`、`disableTracer`、`disableMetricsLog`
- 逻辑服 96 流（`NumCPU*8`）；logic2 `push-size=64`、`push-workers=12`、`expected-members=0`（登录即开推）
- bench2 用 `run.bat on|off` / `run.sh [on|off]` 切换 `config_batch_on.yaml` / `config_batch_off.yaml`（`batchPush: true|false`）

### bench1：client→sgate→logic 转发

| 协议 | 总转发量(10s) | 平均速率 | 登录失败 | droppedAuth |
| --- | ---: | ---: | ---: | ---: |
| TCP | 4,260,438 | **423,989/s** | 0 | 0 |
| WebSocket | 4,264,923 | **424,967/s** | 0 | 0 |

### bench2：logic→sgate→client 推送

| 协议 | batchPush | 平均接收速率 | 连接失败 | droppedAuth |
| --- | --- | ---: | ---: | ---: |
| TCP | true | **1,424,357/s**（总接收 14,263,061） | 0 | 0 |
| TCP | false | 888,463/s（2 轮中位） | 0 | 0 |
| WebSocket | true | **1,372,644/s**（总接收 13,755,700） | 0 | 0 |
| WebSocket | false | 855,332/s（2 轮中位） | 0 | 0 |

- 测量日期 2026-10-09（本机后台有负载）；安静后台时段参考：bench1 TCP 498,490/s、WS 527,297/s，bench2 on TCP 1,934,329/s、WS 1,713,708/s
- 校验：各轮 `connections failed=0`、`droppedAuth=0`，日志无 401 / authfail / login-key 拒绝
- etcd key 使用 protocol 枚举：`/services/{belong}/{ServerTypeName}:{zone}/{instanceId}`（如 `SGATE` / `LOGINSERVER` / `LOGICSERVER`）

