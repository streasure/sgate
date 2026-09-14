# monitor.ps1 - 监控网关状态
param([int]$Duration = 35, [int]$Interval = 2)
$endTime = (Get-Date).AddSeconds($Duration)
Write-Host "时间,内存MB,CPU%,句柄数,连接数,Goroutines"
while ((Get-Date) -lt $endTime) {
    $proc = Get-Process -Name sgate -ErrorAction SilentlyContinue
    if ($proc) {
        $mem = [math]::Round($proc.WorkingSet64 / 1MB, 1)
        $cpu = $proc.CPU
        $handles = $proc.HandleCount
        # 获取网关连接数
        $conns = (Get-NetTCPConnection -State Established -ErrorAction SilentlyContinue | Where-Object { $_.LocalPort -ge 48080 -and $_.LocalPort -le 49050 }).Count
        # 获取 /stats 端口的指标
        $stats = Invoke-WebRequest -Uri "http://127.0.0.1:8081/stats" -TimeoutSec 1 -ErrorAction SilentlyContinue
        $goroutines = "-"
        if ($stats) {
            $json = $stats.Content | ConvertFrom-Json
            $goroutines = $json.goroutines
            $conns = $json.connections
        }
        Write-Host "$(Get-Date -Format 'HH:mm:ss'),${mem},${cpu},${handles},${conns},${goroutines}"
    }
    Start-Sleep -Seconds $Interval
}
