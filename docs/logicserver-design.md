# logicserver 设计案（含 loginserver 仓骨架方案）

> 状态：**已评审定稿**（2026-11-17 按最佳实践裁定：§1 五条照准 + §3.4/§3.5/§4.3 三处修订；未动任何代码）
> 背景：sgate 终态三铁律（无运行时可变包状态 / 包边界=平面边界 / wire 唯一权威=protocol 仓）收口后，
> 需要定两件事：① logicserver（真实逻辑服）归属与实现位置；② 独立服务仓的标准骨架。
> 关联：[architecture §2.4/§2.7](architecture.md)、[logic-guide](logic-guide.md)、[roadmap](roadmap.md)、[`../todo.md`](../todo.md)

---

## 1. 结论（TL;DR）

1. **logicserver 独立进程、独立服务，不放进 sgate**——判据："它会不会随网关一起发布、一起扩容？不会 → 独立。"
2. **实现位置：`E:\logic` 仓内**（`github.com/streasure/logic`），**SDK 与服务同仓**：SDK 根包 + `cmd/logicserver` 服务入口，不另建新仓。
3. **版本治理走 `replace`，不打 tag**（已定决策）：sgate 维持 `replace github.com/streasure/logic => ../logic`，开发期同级目录布局；发版 tag 延后。
4. **loginserver 已是完整的独立服务仓**，其骨架（§5）即独立服务仓的标准方案；logicserver 骨架（§3）与之对称映射。
5. 本稿只定方案与骨架，**实施分阶段进行**（§6），每阶段单独中文提交、五门禁全绿。

---

## 2. 归属决策：为什么独立、为什么不合进 sgate

### 2.1 现状事实（已核实）

| 事实 | 证据 |
| --- | --- |
| `E:\logic` 目前是**纯库**，无 `main`/`cmd/` | 文件清单：`service.go server.go handler.go dialer.go config.go reliable.go` + 测试 |
| 真实 logicserver **尚不存在**，要做的是"新建服务"而非"从 sgate 搬迁" | 同上 |
| sgate 全仓 import `streasure/logic` 的**只有 `bench/logic_*` 7 个 main** | `cmd/gateway`、`internal/*` **零依赖**——铁律"网关不 import 业务"物理成立 |
| SDK 自身依赖 protocol `v1.0.4` + util + grpc，无业务依赖 | `E:\logic/go.mod` |
| sgate `go.mod` 已有 `replace => ../logic`（`v0.0.0` 未发版） | sgate go.mod |

### 2.2 独立的理由

1. **部署单元不同**：网关是连接密集型（百万连接、FD/内存敏感、改动须全套压测门禁）；logicserver 是 CPU 密集型业务。负载曲线不同，混进程只能按最坏情况整体扩容，且业务 OOM 会连坐网关。flip 模型 + 消费方接口（`LogicClientProvider` 等）本就按 N 网关 × M logic 多对多设计。
2. **依赖方向会烂**：sgate 铁律"网关不 import 业务"。同仓后业务类型对 `dataplane`/`logiclink` 就在手边，三平面单向依赖撑不住几次迭代。物理隔离是最便宜的约束。
3. **发布节奏不同**：网关改动 = 五门禁 + 满载复验 + 回归阈值；业务高频迭代。合仓让业务改动背重门禁，或让门禁形同虚设。
4. **拆分成本已付清**：SDK 已迁出、protocol 已独立、bench 假 logic 已验证"独立进程 + etcd 注册 + flip 拨入"全链路可行。真实 logicserver 与 `bench/logic2_ws` 的差别只是 handler 换成业务——现在合仓是把已付成本退回去。
5. **与 loginserver 对称**：拓扑本就是 `sgate / loginserver / (未来) logicserver` 三服务 + `protocol` 共享契约，logicserver 独立只是补齐已有格局。

### 2.3 边界判据（写死）

- **网关进程里**：连接、鉴权、限流、分流、帧处理（帧解析只在 protocol 仓）。
- **logicserver 里**：业务 handler、分组语义、ACK 窗口与重传、状态下发。
- **谁都不碰对方的**：网关不写业务；logic 不解析帧、不过滤、不持连接状态。

---

## 3. 实现位置：`E:\logic` 仓（SDK + 服务同仓）

### 3.1 为什么同仓而不是新立 logicserver 仓

1. **SDK 与服务一起演进**：服务是 SDK 的第一个真实消费者（dogfooding），API 缺口当轮补齐，不必两仓联动两笔提交。
2. **一个 module 一条发布链**：`github.com/streasure/logic` 根包即 SDK；服务只是 `cmd/` 下的一个 main，不需要第二个 go.mod、第二套 `update-gomod.bat`。
3. **sgate 引用零改动**：`replace => ../logic` 照旧，bench 工具链照旧。
4. 仓名与模块名已是 `logic`，语义上"逻辑服的仓"放它的服务天经地义。

### 3.2 目标目录骨架

```
E:\logic\
├── go.mod / go.sum                 # module github.com/streasure/logic（现有）
├── update-gomod.bat                # 跨仓工具链（现有）
├── README.md                       # SDK 文档（现有，补服务运行说明）
│
│── SDK 根包 logic（现有，保持零业务）
├── service.go  server.go  handler.go  dialer.go  config.go  reliable.go
├── *_test.go
│
│── 服务与业务（新增）
├── cmd/
│   └── logicserver/
│       └── main.go                 # 组合根（唯装配点）：flag → tlog → config → NewService → 注册 → Run
├── internal/
│   ├── config/                     # Config 结构 + Load + validate（显式传参，零包级全局态）
│   ├── metrics/                    # /counters 指标端点（PushMetrics 快照 + 进程指标）
│   └── handler/                    # 业务 cmd handler（经 svc.RegisterProto 挂接，不进 SDK 根包）
│
├── config/
│   ├── logicserver.yaml            # 常规配置
│   ├── logicserver_stress.yaml     # 压测配置（限流/准入全关）
│   ├── tlog.yaml / tlog_stress.yaml
├── scripts/
│   ├── start.bat / start-linux.sh
├── docs/
│   └── architecture.md             # 服务架构（对齐 loginserver 体例）
└── bin/                            # 构建产物（gitignore）
```

**边界规则**：SDK 根包 `logic` 永不 import `cmd/`/`internal/`（方向单向：服务 → SDK）；业务 handler 只能经 `RegisterProto` / `svc.Server()` 公开 API 挂接，不允许伸手进 SDK 内部状态。

### 3.3 `cmd/logicserver/main.go` 装配（目标形态）

```go
flag: -conf / -logger / -version          // 对齐 loginserver/sgate 惯例
tlog.Init(*loggerConf); defer tlog.Close() // 日志最先，全程 tlog
cfg := config.Load(*conf)                  // 显式传参，validate required
svc := logic.NewService(
    logic.WithListenPort(strconv.Itoa(cfg.Ports.GrpcPort)),
    logic.WithServiceID(cfg.ServerID),
    logic.WithServerType(cfg.ServerType),     // etcd 键 {belong}/SERVER_TYPE_LOGICSERVER:{zone}
    logic.WithBelong(cfg.Belong),
    logic.WithZone(cfg.Zone),
    logic.WithEtcdEndpoints(cfg.Etcd.Endpoints...), // 注册与发现同源，首项回退单端点
    logic.WithShardCount(cfg.ShardCount),
    logic.WithReliability(cfg.Reliability), // 可靠推送窗口/重传参数进配置
)
internal/handler.Register(svc, cfg)         // 业务注册（RegisterProto/JoinGroup/...）
go internal/metrics.Serve(cfg.Ports.MetricsAddr, svc)  // /counters + /healthz + /readyz
go upprof/pprof 端口                        // 可选
svc.Run()                                   // Start + SIGINT/SIGTERM → 优雅停机
```

与 loginserver 的差异：loginserver 用 util `component.NewContainer` 装配十余个组件；logicserver 的**服务主体由 SDK `Run()` 承载**，伴生组件（pprof/指标）直接 goroutine 即可，**不强套容器**——骨架从简，后续组件多了再引入。

### 3.4 新仓空白的方案

| 空白 | 现状 | 方案 |
| --- | --- | --- |
| **配置** | bench 全硬编码（`WithEtcd("http://127.0.0.1:2379")`、zone/id 写死） | `internal/config`：结构体 + `Load` + `validate`（照抄 loginserver `config.go` 模式：required/default 标签）；常规/压测双套 yaml；**显式传参**（按 sgate 终态铁律，不学 loginserver 旧式包级 `GetConfig`） |
| **优雅 drain** | SDK `Stop()` 已实现：注销 etcd → 关全部流 → `GracefulStop` 5s 超时兜底 → 关 listener（`service.go` L143~175，流必须先关否则 SIGINT 挂死） | 缺口只在**业务在途宽限**：`main` 收到信号后先停收业务（handler 拒新）、等 `PushReliable` 队列与在途 handler 排空（可配宽限时长），再调 `svc.Stop()`；SDK 停机顺序不动 |
| **指标端点** | `PushMetrics.GetSnapshot()` 仅内存计数；`bench/logic_udp` 的 `/counters` 是雏形 | `internal/metrics`：HTTP 暴露 `PushMetricsSnapshot`（acked/retransmits/giveUps/pushed/…）+ members + 进程指标，供网关侧 `/stats` 三方对账（沿用压测对账口径） |
| **健康检查探针** | 无（bench 进程无探针概念） | 同端点分离两路：`/healthz` liveness（进程活着即 200，只用于重启判定）；`/readyz` readiness = **etcd 注册成功 且 flip 已拨入 ≥1 个网关流**（未就绪时不应被发现/摘流）。探针不依赖业务 handler，避免业务假死被误判为可重启 |

### 3.5 版本治理：`replace` 不打 tag（已定决策）

- **现状布局**：`E:\` 下平级仓 `sgate / logic / protocol / util / treasure-slog / loginserver`，各仓 go.mod 均 `replace => ../xxx`（sgate、loginserver、logic 三仓一致，已核实）。
- **决策**：logic SDK 不打 tag，sgate 维持 `replace => ../logic`；protocol 已发 `v1.0.4` 也照样被 replace 覆盖，同款处理。
- **约束（接受的代价）**：
  1. CI/他机必须**同级目录 checkout**（或 `go.work`），否则 replace 断链——跨仓集成冒烟（§4.3）本就需要同级布局；
  2. SDK API 变更**即刻影响** sgate bench：改 SDK 必须同轮跑 sgate 五门禁 + 相关 bench 编译；
  3. 发版治理（tag + changelog）延后，记入 `todo.md` 候选项（§7）；
  4. **无 tag 期的 API 纪律（替代语义化版本的安全网）**：SDK 导出面 **additive-only**——
     只增不改不删；确需破坏性变更时，必须同轮修改 sgate `bench/logic_*` 全部编译通过，
     并在提交信息中显式标注"SDK 破坏性变更"；`docs/logic-guide.md` 同步更新。

### 3.6 sgate 侧配套（实施时改，本稿不动）

- `docs/architecture.md` §2.7 补一句"logicserver 服务实现在 logic 仓 `cmd/logicserver`"；
- `docs/logic-guide.md` 增加"运行服务"一节（指向 logic 仓）；
- `bench/logic_*` 定位明文化：**SDK 官方示例 + 集成测试替身**，真实服务从其模式 fork。

---

## 4. 跨仓契约与 CI

### 4.1 三方契约分层

| 契约 | 权威位置 | 说明 |
| --- | --- | --- |
| **wire 契约** | `github.com/streasure/protocol`（`v1.0.4`，replace） | 命令码、`StreamData`、flip 握手元数据、`MessageFrame`——sgate/logic/客户端三方唯一依赖它，互不依赖 |
| **行为契约** | sgate `docs/logic-guide.md` | 分组语义、ACK 窗口、断线清窗、心跳保活、推送 API——logic 仓 README 只链接不复制 |
| **接入契约** | sgate `docs/client-guide.md` / `docs/udp-ws-design.md` | 客户端侧 |

### 4.2 依赖方向（逻辑视图）

```
protocol ← sgate          （网关：帧解析/命令码）
protocol ← logic          （逻辑服：StreamData 收发、命令处理）
sgate  --(gRPC flip 流, etcd 发现)-->  logicserver      # logic 主动拨入网关
sgate  --(gRPC ValidateLoginToken)-->  loginserver      # 登录校验
loginserver --(gRPC Kick, 单向)-->  sgate               # 踢人执行
```

零反向：sgate 不 import logic SDK；loginserver 不 import sgate；三者只共享 protocol + util。

### 4.3 跨仓集成冒烟（当前 CI 缺口）

- sgate CI 跑不到真实 logicserver；补法：把现有 bench 工具链固化成冒烟脚本——
  `gateway + loginserver + logicserver(新) + bench1/client` 四进程起停 + `/stats`↔`/counters` 对账。
- 材料现成：`bench/logic_*`（服务范例）、`bench/client_*`、`bench/udp_*`、`config/bench/*`。
- **版本钉扎**：冒烟脚本 checkout 各兄弟仓到**同一批 commit**（或记录 `git rev-parse HEAD` 清单），
  防止"protocol 改了、sgate/logic 没拉"造成的假绿；replace 布局下各仓独立漂移是主要风险源。
- 归入 `todo.md` 第 1 节「Linux CI（P2-4）」一并推进。

---

## 5. loginserver 仓骨架方案（现状即方案，供审阅）

> `E:\loginserver`（`github.com/streasure/loginserver`）已是完整独立仓，本节把它的骨架**写成方案**：
> 既作为现状存档，也作为 logicserver（§3）对齐的模板。

### 5.1 定位与接口

- **职责**：登录/版本接口（HTTP）、登录令牌校验（gRPC，供 sgate）、ACL 黑白名单与封禁角色、封禁踢人（单向 Kick 模型）。
- **端口**：HTTP `10001` / gRPC `10002` / pprof `10003` / load 状态 `10004`（可选）。
- **与 sgate 的接口（单向模型）**：

| 方向 | RPC/机制 | 说明 |
| --- | --- | --- |
| sgate → loginserver | `ValidateLoginToken` | LoginGate 过门校验；携带 `sgate_id/conn_id/client_ip` 写 Redis 踢人路由索引（TTL 24h）；封禁在此拦截 |
| sgate → loginserver | `BatchValidateLoginTokens` | 周期安全网（`loginValidation.batchInterval`），MGET 预取合并回源 |
| loginserver → sgate | `KickIP` / `KickAccounts` | 经 etcd `SGATE:{zone}` 发现实例、`x-internal-token` 鉴权；封禁权威在 loginserver MySQL，sgate 纯执行器、失败不回滚 |

### 5.2 目录骨架

```
E:\loginserver\
├── cmd/loginserver/main.go      # 组合根（唯装配点）
├── internal/
│   ├── config/                  # Config 结构 + LoadConfig + validate（含 config_test）
│   ├── component/               # etcd 组件、load（uload 准入）组件
│   ├── dmodel/                  # MySQL 仓：封禁角色 / IP 黑白名单 / 服务器列表
│   ├── rmodel/                  # Redis 仓：账号 / loginToken / 会话索引（sess:* 四键）
│   ├── service/                 # 业务服务：service_login / service_acl / login_grpc_server /
│   │                            #   sgate_client（Kick 客户端池）/ grpc_admin_auth
│   ├── httphandler/             # gin 路由（blank import 注册）：login/version/validate/acl/session
│   ├── pkg/dto/                 # DTO 与业务错误码（1004~1007）
│   └── version.go               # ldflags 注入 Version（现 v1.0.2）
├── config/                      # 4 件套：loginserver{,_stress}.yaml + tlog{,_stress}.yaml
├── docs/architecture.md         # 单向 Kick 模型 + 会话索引 + 封禁链路
├── scripts/                     # start.bat / start-linux.sh（压测配置可传参）
├── sql/                         # create_database.sql / create_tables.sql
├── tools/                       # httpstress / grpcstress 压测客户端
└── update-gomod.bat             # replace => ../protocol ../util ../treasure-slog
```

### 5.3 装配顺序（`main.go` 容器 Add 顺序，已核实）

```
tlog.Init → config.LoadConfig → component.NewContainer
  ① uperd.New            # GC/内存软上限，Order 最先，Init 时先于一切分配
  ② uredis.Init          # redis（rmodel 依赖）
  ③ ugorm.NewComponent   # MySQL（dmodel/ACL 快照依赖）
  ④ service.GetAclService # 启动全量加载黑白名单/封禁快照（失败即失败启动）
  ⑤ service.NewLoginGrpcServer
  ⑥ internalcomponent.NewEtcdComponent
  ⑦ service.RegisterSgateClients + udiscovery.GetRegistry  # Kick 客户端（发现不可用即启动失败）
  ⑧ NewLoadComponent + ugin.NewComponent(HTTP, WithPreMiddleware(uload.Middleware()))
  ⑨ upprof.NewUPprofComponent
→ container.Serve()   # etcd 由 util 默认排最后启动、销毁时最先注销
```

**可复用的骨架原则**：日志最先且唯一（tlog）→ 配置显式加载 → 依赖存储先于业务组件 → 对外 RPC/HTTP 后于依赖 → 伴生端点最后；销毁严格逆序。

### 5.4 工程规范（两仓共同遵守）

- 日志只用 `tlog`（`fmt.Print` 仅限 `-version` 这类非日志输出）；panic 恢复用 `panicutil`，tag `package.component`。
- 五门禁：`gofmt -l` / `go build -p 1 -ldflags="-s -w"` / `go vet ./...` / `go test -p 1 ./... -count=1` / `staticcheck ./...`。
- 每仓每笔单独中文提交（含验证结论）；本地不推送，推送须经确认。
- 压测配置关准入/限流、日志仅 error；bench 不走 HTTP login、不启用 loginserver token 校验。

### 5.5 logicserver 骨架映射表

| loginserver（模板） | logicserver（§3 目标） | 差异说明 |
| --- | --- | --- |
| `cmd/loginserver/main.go` 组合根 | `cmd/logicserver/main.go` | logicserver 主体走 SDK `Run()`，伴生组件 goroutine 化，不套容器 |
| `internal/config`（包级 `GetConfig`） | `internal/config`（**显式传参**） | 新仓守 sgate 终态铁律；loginserver 旧式不主动改造（§7.3） |
| `internal/service` 自带 gRPC server | **SDK 根包 `logic.NewService` 承载**（已有 flip 流/etcd/停机） | 最大差异：logicserver 不自写 gRPC 服务端 |
| `internal/{dmodel,rmodel}` 存储层 | 业务需要时再建（首版无存储） | 按需 |
| `internal/httphandler` + `pkg/dto` | `internal/handler`（cmd 分发）+ `internal/metrics` | logicserver 无对外 HTTP 业务面，只有指标 |
| `config/` 4 件套 | 同款 4 件套 | 一致 |
| `scripts/` `docs/` `sql/` `tools/` | `scripts/` `docs/`（`sql`/`tools` 按需） | 首版从简 |
| 版本 ldflags 注入 | 同款 | 一致 |

---

## 6. 实施步骤（分阶段，评审通过后排期）

| 阶段 | 内容 | 验收 |
| --- | --- | --- |
| 0 | 本稿评审（不动代码） | ✅ 已过（2026-11-17，§1 五条照准 + 三处最佳实践修订） |
| 1 | `E:\logic` 建 `cmd/logicserver` + `internal/config` 最小可运行骨架（登录入组 + LoginReq/LoginAck 应答 handler，模式照 `bench/logic2_ws`；无协议 echo 命令码，不自造 wire 契约） | ✅ 已过（2026-11-17，logic `8ea8750`）：logic/sgate 两仓五门禁绿；etcd 注册键实查存在、flip 建立、bench1_ws 100/100 登录 0 失败 1.37M 消息、handler 侧 `login gate bound`×100 且 0 unregistered |
| 2 | 配置 4 件套 + `scripts/` + `docs/architecture.md` + README 补运行说明 | 压测配置可起、常规配置可起 |
| 3 | `internal/metrics` `/counters` 端点 + 业务在途宽限的优雅 drain | 停机无挂死、无丢弃；`/stats`↔`/counters` 对账闭合 |
| 4 | sgate 侧文档配套（§3.6）+ 跨仓冒烟脚本（§4.3） | 四进程冒烟一键过 |
| 5 | （延后）tag 发版治理 | 触发条件：logic SDK 出现第二个仓外消费者 |

每阶段：单独中文提交（写明验证结论）、五门禁全绿、**推送经用户确认**。

## 7. 待办候选（已按此口径补进 `todo.md`）

1. logic SDK 版本治理（tag + changelog，replace 切 require 的时机）——阶段 5 前置；
2. 跨仓集成冒烟基线（§4.3，与 Linux CI P2-4 打包）；
3. loginserver 包级 `GetConfig` 改显式传参——**仅当出现实际痛点（如测试需注入不同配置）才动，
   不主动排期**；新仓（logicserver）一律显式传参，旧代码不为风格买单（童子军军规）。
