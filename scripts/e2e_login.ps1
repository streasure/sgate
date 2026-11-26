# 一次性 e2e：loginserver + gateway + logicserver + client_logincheck
# 断言：登录全链路成功 + logicserver 建号日志(created=true) + 快照应答 + 二登复用(created=false)。
#
# 用法（任意工作目录均可，路径相对本脚本推导）：
#   powershell -ExecutionPolicy Bypass -File scripts\e2e_login.ps1
#
# 前置（缺一不可）：
#   1. 四个二进制在位：
#        go build -o bin/gateway.exe ./cmd/gateway            （sgate 仓库根，建议 -p 1 -ldflags="-s -w"）
#        go build -o bin/client_logincheck.exe ./tools/client_logincheck
#        go build -o bin/logicserver.exe ./cmd/logicserver    （logic 仓库根）
#        go build -o bin/loginserver.exe ./cmd/loginserver    （loginserver 仓库根）
#      注意：logic 仓 scripts/apidiff.sh 会连带清理 gitignored 的 bin/ 产物，
#      跑过 apidiff 后需重建 logicserver.exe。
#   2. 三仓为同级目录布局（sgate/logic/loginserver 并列，设计案 §3.5 约束）。
#   3. 端口空闲：10001(login HTTP) / 8081(gateway admin) / 48080(gate ws) / 6061(logic pprof)。
#
# 副作用：启动前 Stop-Process 杀 gateway/logicserver/loginserver/client_logincheck
# 四个进程（与常驻 e2e 环境共用进程名），结束时再次清理。
[Console]::OutputEncoding = [Text.Encoding]::UTF8
$ErrorActionPreference = "Stop"
$fail = @()
function Assert-True([bool]$c, [string]$m) {
    if ($c) { Write-Host "  PASS  $m" } else { $script:fail += $m; Write-Host "  FAIL  $m" -ForegroundColor Red }
}
function Wait-Port([int]$p, [int]$sec, [string]$n) {
    $d = (Get-Date).AddSeconds($sec)
    while ((Get-Date) -lt $d) {
        if (Get-NetTCPConnection -LocalPort $p -State Listen -ErrorAction SilentlyContinue) { return $true }
        Start-Sleep -Milliseconds 500
    }
    Write-Host "  port $p not listening ($n)" -ForegroundColor Red
    return $false
}

# 三仓同级布局：由本脚本位置（<sgate>/scripts/）推导仓库根与兄弟仓
$sgate = Split-Path $PSScriptRoot -Parent
$sibling = Split-Path $sgate -Parent
$login = Join-Path $sibling "loginserver"
$logic = Join-Path $sibling "logic"

# client_logincheck 的 logutil 拒绝在系统 temp 目录下解析相对日志路径，
# 统一切到 sgate 仓库根执行（Start-Process 均已带显式 WorkingDirectory，不受影响）
Push-Location $sgate

Stop-Process -Name gateway, logicserver, loginserver, client_logincheck -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 1

$logicLog = Join-Path $logic "logs\logicserver.log"
if (Test-Path $logicLog) { Remove-Item $logicLog -Force -ErrorAction SilentlyContinue }

try {
    $p = Start-Process (Join-Path $login "bin\loginserver.exe") -ArgumentList "-conf","config\loginserver.yaml","-logger","config\tlog.yaml" -WorkingDirectory $login -WindowStyle Hidden -PassThru
    if (-not (Wait-Port 10001 30 "loginserver")) { throw "loginserver 启动失败" }

    $p = Start-Process (Join-Path $sgate "bin\gateway.exe") -ArgumentList "-conf","config\bench\batch-off.yaml","-logger","config\log\bench.yaml" -WorkingDirectory $sgate -WindowStyle Hidden -PassThru
    $live = $false
    $d = (Get-Date).AddSeconds(30)
    while ((Get-Date) -lt $d) {
        try { $r = Invoke-WebRequest "http://127.0.0.1:8081/live" -TimeoutSec 3 -UseBasicParsing; if ($r.StatusCode -eq 200) { $live = $true; break } } catch {}
        Start-Sleep -Milliseconds 500
    }
    if (-not $live) { throw "gateway /live 未就绪" }

    $p = Start-Process (Join-Path $logic "bin\logicserver.exe") -ArgumentList "-conf","config\logicserver.yaml","-logger","config\tlog.yaml" -WorkingDirectory $logic -WindowStyle Hidden -PassThru
    $ready = $false
    $d = (Get-Date).AddSeconds(60)
    while ((Get-Date) -lt $d) {
        try { $r = Invoke-WebRequest "http://127.0.0.1:6061/readyz" -TimeoutSec 5 -UseBasicParsing; if ($r.StatusCode -eq 200) { $ready = $true; break } } catch {}
        Start-Sleep -Milliseconds 500
    }
    if (-not $ready) { throw "logicserver /readyz 未就绪" }

    $healthy = $false
    $d = (Get-Date).AddSeconds(15)
    while ((Get-Date) -lt $d) {
        try { $h = Invoke-RestMethod "http://127.0.0.1:8081/health" -TimeoutSec 3; if ($h.status -eq "healthy") { $healthy = $true; break } } catch {}
        Start-Sleep -Milliseconds 500
    }
    Assert-True $healthy "gateway /health = healthy (flip 已建立)"
    Write-Host "== 三进程就绪，跑 client_logincheck =="
    Start-Sleep -Seconds 1  # 等日志 flush（async 100ms）

    $cc = Join-Path $sgate "bin\client_logincheck.exe"
    $ccCfg = Join-Path $sgate "config\log\bench.yaml"
    & $cc -loginserver "127.0.0.1:10001" -sgate "127.0.0.1:48080" -server-id "logic1-ws" -config $ccCfg
    Assert-True ($LASTEXITCODE -eq 0) "client_logincheck 退出码=0（LoginGate + LoginAck 全链路）"

    Start-Sleep -Seconds 2  # 等 tlog async flush
    $log = ""
    if (Test-Path $logicLog) { $log = Get-Content $logicLog -Raw }
    Assert-True ($log -match "logic login .*created=true") "logicserver 建号日志 created=true 首登"
    Assert-True ($log -match "accounts=1") "内存账号仓 accounts=1"

    # 二登同账号：断言不重建（created=false）
    & $cc -loginserver "127.0.0.1:10001" -sgate "127.0.0.1:48080" -server-id "logic1-ws" -config $ccCfg
    Assert-True ($LASTEXITCODE -eq 0) "client_logincheck 二登退出码=0"
    Start-Sleep -Seconds 2
    $log = ""
    if (Test-Path $logicLog) { $log = Get-Content $logicLog -Raw }
    Assert-True ($log -match "created=false accounts=1") "二登复用档案 created=false accounts=1"
} catch {
    $fail += "EXCEPTION: $($_.Exception.Message)"
    Write-Host "  EXCEPTION: $($_.Exception.Message)" -ForegroundColor Red
} finally {
    Stop-Process -Name gateway, logicserver, loginserver, client_logincheck -Force -ErrorAction SilentlyContinue
    Pop-Location
    Write-Host "== 清理完成 =="
}

Write-Host ""
if ($fail.Count -eq 0) { Write-Host "E2E PASSED" -ForegroundColor Green; exit 0 }
Write-Host "E2E FAILED ($($fail.Count)):" -ForegroundColor Red
$fail | ForEach-Object { Write-Host "  - $_" -ForegroundColor Red }
exit 1
