<#
.SYNOPSIS
  跨仓集成冒烟（docs/logicserver-design.md §4.3）：四进程一键起停 + 对账。

.DESCRIPTION
  构建并拉起 loginserver + sgate gateway + logicserver + bench1_ws 四进程，
  等待 flip（logicserver /readyz 200）后跑 bench1 上行负载，随后做 /stats ↔
  /counters 对账断言，最后优雅清理（不动 etcd/MySQL/Redis 常驻服务）。

  版本钉扎：运行前把 sgate/logic/loginserver/protocol/util 五个仓的
  `git rev-parse HEAD` 清单写入 logs/smoke_versions.txt（replace 布局下各仓
  独立漂移是假绿主因，清单用于事后归因）。

  登录校验按 AGENTS.md Bench 规则保持 loginValidation.enabled=false（冒烟用
  config/bench/batch-off.yaml，对账口径 batch-off）；loginserver 参与的是
  「起停 + etcd 注册 + MySQL/Redis 初始化」链路，不走令牌校验热路径。

.PARAMETER Parallel
  bench1 并发连接数（登录数），默认 100。

.PARAMETER Duration
  bench1 上行洪泛时长，默认 5s。

.PARAMETER SkipBuild
  跳过四仓构建（调试用，复用已有 bin）。

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File scripts\smoke-logicserver.ps1
  powershell -File scripts\smoke-logicserver.ps1 -Parallel 50 -Duration 3s
#>
param(
    [int]$Parallel = 100,
    [string]$Duration = "5s",
    [switch]$SkipBuild
)

[Console]::OutputEncoding = [Text.Encoding]::UTF8   # 中文输出按 UTF-8 落到捕获端
$ErrorActionPreference = "Stop"
$smokeFailed = [System.Collections.Generic.List[string]]::new()
$started = [System.Collections.Generic.List[System.Diagnostics.Process]]::new()
$benchProc = $null

function Assert-True {
    param([bool]$Cond, [string]$Msg)
    if ($Cond) {
        Write-Host "  PASS  $Msg"
    } else {
        $script:smokeFailed.Add($Msg)
        Write-Host "  FAIL  $Msg" -ForegroundColor Red
    }
}

function Wait-Port {
    param([int]$Port, [int]$Sec, [string]$Name)
    $deadline = (Get-Date).AddSeconds($Sec)
    while ((Get-Date) -lt $deadline) {
        if (Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue) {
            Write-Host "  port $Port listening ($Name)"
            return $true
        }
        Start-Sleep -Milliseconds 500
    }
    Write-Host "  FAIL  port $Port not listening within ${Sec}s ($Name)" -ForegroundColor Red
    return $false
}

function Invoke-Json {
    param([string]$Url)
    try {
        return Invoke-RestMethod -Uri $Url -TimeoutSec 5
    } catch {
        return $null
    }
}

$sgate = Split-Path $PSScriptRoot -Parent                 # scripts/ 的上级 = sgate 根
$parent = Split-Path $sgate -Parent
$logicDir = Join-Path $parent "logic"
$loginDir = Join-Path $parent "loginserver"
$protoDir = Join-Path $parent "protocol"
$utilDir = Join-Path $parent "util"
$logsDir = Join-Path $sgate "logs"

try {
    # ---------- 0. 前置依赖（常驻服务，不归本脚本管理） ----------
    Write-Host "== [0/6] 前置依赖 =="
    foreach ($dep in @(
        @{ P = 2379; N = "etcd" },
        @{ P = 3306; N = "MySQL" },
        @{ P = 6379; N = "Redis" }
    )) {
        if (-not (Get-NetTCPConnection -LocalPort $dep.P -State Listen -ErrorAction SilentlyContinue)) {
            throw "前置依赖未就绪：$($dep.N) 端口 $($dep.P) 未监听"
        }
        Write-Host "  $($dep.N) ok (:$($dep.P))"
    }

    # ---------- 1. 版本钉扎（replace 布局防各仓漂移假绿） ----------
    Write-Host "== [1/6] 版本钉扎 =="
    $repos = [ordered]@{
        sgate      = $sgate
        logic      = $logicDir
        loginserver = $loginDir
        protocol   = $protoDir
        util       = $utilDir
    }
    $manifest = @("# smoke-logicserver 版本清单 $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')")
    foreach ($name in $repos.Keys) {
        $dir = $repos[$name]
        $head = ""
        if (Test-Path (Join-Path $dir ".git")) {
            $eap = $ErrorActionPreference; $ErrorActionPreference = "Continue"  # git stderr 不因 Stop 中断
            $head = (& git -C $dir rev-parse HEAD 2>&1 | Out-String).Trim()
            $ErrorActionPreference = $eap
            if ($LASTEXITCODE -ne 0) { $head = "" }
        }
        if (-not $head) { $head = "(not a git repo)" }
        $manifest += "$name $head"
        Write-Host "  $name $head"
    }
    if (-not (Test-Path $logsDir)) { New-Item -ItemType Directory -Path $logsDir | Out-Null }
    $manifestPath = Join-Path $logsDir "smoke_versions.txt"
    $manifest | Set-Content -Path $manifestPath -Encoding ASCII
    Write-Host "  manifest -> $manifestPath"

    # ---------- 2. 构建四进程 ----------
    Write-Host "== [2/6] 构建 =="
    if ($SkipBuild) {
        Write-Host "  (skip by -SkipBuild)"
    } else {
        $builds = @(
            @{ Dir = $sgate;   Out = "bin\gateway.exe";    Tgt = ".\cmd\gateway" },
            @{ Dir = $sgate;   Out = "bin\bench1_ws.exe";  Tgt = ".\bench\bench1_ws" },
            @{ Dir = $logicDir; Out = "bin\logicserver.exe"; Tgt = ".\cmd\logicserver" },
            @{ Dir = $loginDir; Out = "bin\loginserver.exe"; Tgt = ".\cmd\loginserver" }
        )
        foreach ($b in $builds) {
            Push-Location $b.Dir
            try {
                go build -o $b.Out $b.Tgt
                if ($LASTEXITCODE -ne 0) { throw "go build failed: $($b.Dir) $($b.Tgt)" }
            } finally { Pop-Location }
            Write-Host "  built $($b.Out)"
        }
    }
    foreach ($exe in @("bin\gateway.exe", "bin\bench1_ws.exe")) {
        if (-not (Test-Path (Join-Path $sgate $exe))) { throw "missing $exe" }
    }
    if (-not (Test-Path (Join-Path $logicDir "bin\logicserver.exe"))) { throw "missing logic bin\logicserver.exe" }
    if (-not (Test-Path (Join-Path $loginDir "bin\loginserver.exe"))) { throw "missing loginserver bin\loginserver.exe" }

    # ---------- 3. 清残留（只杀冒烟四进程，不动 etcd/MySQL/Redis） ----------
    Write-Host "== [3/6] 清理残留 =="
    Stop-Process -Name gateway, bench1_ws, logicserver, loginserver -Force -ErrorAction SilentlyContinue
    # 等端口释放（最多 5s），仍占用则说明有非冒烟进程占端口
    $busy = @()
    $deadline = (Get-Date).AddSeconds(5)
    do {
        Start-Sleep -Milliseconds 500
        $busy = @()
        foreach ($p in 48081, 8081, 50051, 50053, 6061, 10001, 10002) {
            if (Get-NetTCPConnection -LocalPort $p -State Listen -ErrorAction SilentlyContinue) { $busy += $p }
        }
    } while ($busy.Count -gt 0 -and (Get-Date) -lt $deadline)
    if ($busy.Count -gt 0) {
        throw "端口仍被占用（非冒烟进程残留）: $($busy -join ', ')，请释放后重跑"
    }
    Write-Host "  ports clean"

    # ---------- 4. 启动：loginserver → gateway → logicserver ----------
    Write-Host "== [4/6] 启动四进程 =="
    $p = Start-Process -FilePath (Join-Path $loginDir "bin\loginserver.exe") `
        -ArgumentList "-conf", "config\loginserver.yaml", "-logger", "config\tlog.yaml" `
        -WorkingDirectory $loginDir -WindowStyle Hidden -PassThru
    $started.Add($p)
    if (-not (Wait-Port -Port 10001 -Sec 30 -Name "loginserver HTTP")) { throw "loginserver 启动失败" }

    $p = Start-Process -FilePath (Join-Path $sgate "bin\gateway.exe") `
        -ArgumentList "-conf", "config\bench\batch-off.yaml", "-logger", "config\log\bench.yaml" `
        -WorkingDirectory $sgate -WindowStyle Hidden -PassThru
    $started.Add($p)
    # /live 恒 200 = HTTP 栈已起。注意 /health 含「Logic server connected」判据，
    # 在 logicserver 拉起前恒 503，不能用作本步的就绪门（留到 logic 接入后断言）。
    $gwLive = $false
    $deadline = (Get-Date).AddSeconds(30)
    while ((Get-Date) -lt $deadline) {
        try {
            $r = Invoke-WebRequest -Uri "http://127.0.0.1:8081/live" -TimeoutSec 3 -UseBasicParsing
            if ($r.StatusCode -eq 200) { $gwLive = $true; break }
        } catch { }
        Start-Sleep -Milliseconds 500
    }
    if (-not $gwLive) { throw "gateway /live 未在 30s 内就绪" }
    Write-Host "  gateway up (/live 200)"

    $p = Start-Process -FilePath (Join-Path $logicDir "bin\logicserver.exe") `
        -ArgumentList "-conf", "config\logicserver.yaml", "-logger", "config\tlog.yaml" `
        -WorkingDirectory $logicDir -WindowStyle Hidden -PassThru
    $started.Add($p)
    # /readyz 200 = 已注册 etcd + flip 已建立（否则 503 no gateway stream / not registered）
    $ready = $false
    $deadline = (Get-Date).AddSeconds(60)
    while ((Get-Date) -lt $deadline) {
        try {
            $r = Invoke-WebRequest -Uri "http://127.0.0.1:6061/readyz" -TimeoutSec 5 -UseBasicParsing
            if ($r.StatusCode -eq 200) { $ready = $true; break }
        } catch { }
        Start-Sleep -Milliseconds 500
    }
    if (-not $ready) { throw "logicserver /readyz 未在 60s 内就绪（注册/flip 失败）" }
    Write-Host "  logicserver ready (registered + gateway stream up)"

    # flip 建立后 /health 判据（含 Logic server connected）应转 healthy
    $gwHealthy = $false
    $deadline = (Get-Date).AddSeconds(15)
    while ((Get-Date) -lt $deadline) {
        $h = Invoke-Json "http://127.0.0.1:8081/health"
        if ($h -and $h.status -eq "healthy") { $gwHealthy = $true; break }
        Start-Sleep -Milliseconds 500
    }
    if (-not $gwHealthy) { throw "gateway /health 未转 healthy（逻辑服接入异常）" }
    Write-Host "  gateway healthy (logic connected)"

    # ---------- 5. bench1 负载 + 运行中采样 ----------
    Write-Host "== [5/6] bench1 负载 (parallel=$Parallel duration=$Duration) =="
    $benchOut = Join-Path $logsDir "smoke_bench1.out"
    $benchErr = Join-Path $logsDir "smoke_bench1.err"
    Remove-Item $benchOut, $benchErr -Force -ErrorAction SilentlyContinue
    $benchProc = Start-Process -FilePath (Join-Path $sgate "bin\bench1_ws.exe") `
        -ArgumentList "-addr", "127.0.0.1:48081", "-duration", $Duration,
                      "-parallel", "$Parallel", "-server-id", "logic1-ws" `
        -WorkingDirectory $sgate -WindowStyle Hidden -PassThru `
        -RedirectStandardOutput $benchOut -RedirectStandardError $benchErr

    $sawActive = $false
    $sawSessions = $false
    $gwActiveMax = 0
    $lcSessionsMax = 0
    $deadline = (Get-Date).AddSeconds(120)
    while (-not $benchProc.HasExited -and (Get-Date) -lt $deadline) {
        $stats = Invoke-Json "http://127.0.0.1:8081/stats"
        $counters = Invoke-Json "http://127.0.0.1:6061/counters"
        if ($stats) {
            if ($stats.activeConnections -gt $gwActiveMax) { $gwActiveMax = $stats.activeConnections }
            if ($stats.activeConnections -ge $Parallel) { $sawActive = $true }
        }
        if ($counters) {
            if ($counters.service.sessions -gt $lcSessionsMax) { $lcSessionsMax = $counters.service.sessions }
            if ($counters.service.sessions -ge $Parallel) { $sawSessions = $true }
        }
        Start-Sleep -Milliseconds 500
    }
    if (-not $benchProc.HasExited) {
        Stop-Process -Id $benchProc.Id -Force -ErrorAction SilentlyContinue
        throw "bench1 超时 120s 未退出"
    }
    Start-Sleep -Seconds 2   # 等合批缓冲 flush（batch-off ~5ms tick）与计数落账

    # ---------- 6. 对账断言 ----------
    Write-Host "== [6/6] /stats <-> /counters 对账 =="
    $benchText = ""
    if (Test-Path $benchOut) { $benchText = Get-Content $benchOut -Raw -ErrorAction SilentlyContinue }
    $loginMatch = [regex]::Match($benchText, "connections logged in: (\d+)")
    $failMatch = [regex]::Match($benchText, "connections failed: (\d+)")
    $sentMatch = [regex]::Match($benchText, "total sent \(client-side\): (\d+)")
    $benchLogin = if ($loginMatch.Success) { [int]$loginMatch.Groups[1].Value } else { -1 }
    $benchFailed = if ($failMatch.Success) { [int]$failMatch.Groups[1].Value } else { -1 }
    $benchSent = if ($sentMatch.Success) { [int]$sentMatch.Groups[1].Value } else { -1 }

    $stats = Invoke-Json "http://127.0.0.1:8081/stats"
    $counters = Invoke-Json "http://127.0.0.1:6061/counters"
    $loginHttp = Get-NetTCPConnection -LocalPort 10001 -State Listen -ErrorAction SilentlyContinue

    # 5.1 客户端登录结果
    Assert-True ($benchFailed -eq 0) "bench1 登录失败数=0 (got $benchFailed)"
    Assert-True ($benchLogin -eq $Parallel) "bench1 登录成功数=$Parallel (got $benchLogin)"

    # 5.2 运行中双侧连接/会话水位（gauge 只在运行窗口有效）
    Assert-True $sawActive "gateway activeConnections 达到 $Parallel (max=$gwActiveMax)"
    Assert-True $sawSessions "logic sessions 达到 $Parallel (max=$lcSessionsMax)"

    # 5.3 逻辑服接入状态
    if ($counters) {
        Assert-True ($counters.service.registered -eq $true) "logic 已注册 etcd (registered=$($counters.service.registered))"
        Assert-True ($counters.service.gatewayStreams -ge 1) "flip 流已建立 (gatewayStreams=$($counters.service.gatewayStreams))"
        Assert-True ($counters.push.totalFailed -eq 0) "logic 推送零失败 (totalFailed=$($counters.push.totalFailed))"
    } else {
        Assert-True $false "logic /counters 可读"
    }

    # 5.4 下行欢迎推送对账（batch-off：totalPushed <-> pushedToClient）
    if ($stats -and $counters) {
        Assert-True ($counters.push.totalPushed -eq $stats.pushedToClient) `
            "下行对账 totalPushed=$($counters.push.totalPushed) == pushedToClient=$($stats.pushedToClient)"
        Assert-True ($stats.droppedTotal -eq 0) "gateway 零丢弃 (droppedTotal=$($stats.droppedTotal))"
        Assert-True ($stats.failed -eq 0) "gateway 转发零失败 (failed=$($stats.failed))"
        if ($benchSent -ge 0) {
            Assert-True ($stats.received -ge $benchSent) `
                "gateway received=$($stats.received) >= 客户端发送 $benchSent"
        }
    } else {
        Assert-True $false "gateway /stats 与 logic /counters 均可读"
    }

    # 5.5 loginserver 存活（四进程之第四进程）
    Assert-True ($null -ne $loginHttp -and -not $started[0].HasExited) "loginserver 存活且 HTTP 10001 监听"
} catch {
    $smokeFailed.Add("EXCEPTION: $($_.Exception.Message)")
    Write-Host "  EXCEPTION: $($_.Exception.Message)" -ForegroundColor Red
} finally {
    # ---------- 清理四进程（etcd/MySQL/Redis 不动） ----------
    if ($benchProc -and -not $benchProc.HasExited) {
        Stop-Process -Id $benchProc.Id -Force -ErrorAction SilentlyContinue
    }
    foreach ($proc in $started) {
        if ($proc -and -not $proc.HasExited) {
            Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
        }
    }
    Stop-Process -Name gateway, bench1_ws, logicserver, loginserver -Force -ErrorAction SilentlyContinue
    Write-Host "== 清理完成（etcd/MySQL/Redis 保持运行）=="
}

Write-Host ""
if ($smokeFailed.Count -eq 0) {
    Write-Host "SMOKE PASSED: 四进程冒烟 + 对账全部通过" -ForegroundColor Green
    exit 0
} else {
    Write-Host "SMOKE FAILED ($($smokeFailed.Count)):" -ForegroundColor Red
    $smokeFailed | ForEach-Object { Write-Host "  - $_" -ForegroundColor Red }
    exit 1
}
