# Changelog

sgate 网关的版本记录。依赖当前一律本地 `replace`，不做 `replace => require`
切换；首个基线 tag 前的早期历史见 git log。

## [Unreleased]

### Added

- 有界化三件套：`routeKeyCache` 上限 65536（`ede9040`）、熔断器 `maxBreakers`
  上限防随机 cmd 换层泄漏（`d04a16d`）、BatchValidate items 携带 `client_ip`
  补 IP 黑名单周期兜底（`d30623d`）
- 下行写路径有界背压：先延迟后丢弃 + 每连接/全局两级上限，合批缓冲硬顶标定
  （`0b7d481`/`28d3f24`）
- UDP 阶段 4 压测工具三件套 + udp-on 配置（`9d6dc7d`）
- `scripts/e2e_login.ps1` 四进程登录链路冒烟（`17cfe4c`）；依赖方向检查
  `scripts/check-deps.sh`（`740643d`）
- 契约测试钉住 `DefaultPreAuthCommands == protocol.CmdLoginGate`（`5de0c04`）

### Changed

- logic SDK 发版治理回填：tag v0.1.0 + apidiff 门禁（`93a8060`）
- `trustedProxies` 与 `KickByIP` 收敛为精确 IP，移除 CIDR（`9a6314c`）
- 分组契约补 `JoinGroup` 单会话 256 组上限说明（`661f386`，配合 logic `fb07839`）
- 切维护踢人一次性语义成文：失败不补踢，与封禁周期兜底区分（`967c07c`）

### Fixed

- 删除鉴权前 UserKey 改绑死代码，固化安全不变量（`034922e`）
- `client_logincheck` 容忍登录窗口异步下行并断言 LoginAck 快照（`a67c550`）
