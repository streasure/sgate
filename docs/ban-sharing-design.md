# 封禁共享设计（已废弃）

本文档基于已删除的 BanStore/banRedis/Pub-Sub 模型，封禁收敛 P1（`eb4b21c`）后不再适用。

**现行方案**：loginserver 权威库（MySQL ACL）+ 单向 KickService + BatchValidate 周期安全网。
详见 `E:\loginserver\docs\ban-convergence-design.md`。

sgate 侧仅保留 KickService 执行器（`KickAccounts`/`KickIP`）与登录前 ValidateLoginToken 拦截。
