# 多 Sgate 协作测试说明（已过时）

本文档基于已删除的 BanStore/banRedis/Pub-Sub 模型编写（封禁收敛 P1 `eb4b21c` 删除）。
`/admin/ban|unban|bans` 端点、`security.banRedis`、事件同步等均已不存在。

**现行模型**：loginserver 权威库 + 单向 KickService + BatchValidate 周期安全网。
多实例封禁一致性由 loginserver `SessionSgatesByAccount` 路由 + sgate 本地 ForEach 兜底覆盖，无需 Redis。

**基线连接测试**（T1）仍可参考：2 sgate + logic1_tcp + bench1_tcp，双侧登录 0 失败、转发消息 0 错误。
封禁相关用例（T2–T7）已不适用，略。
