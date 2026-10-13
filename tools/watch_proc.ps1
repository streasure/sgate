# 采样 sgate 进程 CPU 累计秒数 + RSS -> CSV（CPU% 由相邻行差分计算）
$out = "E:\sgate\logs\churn30_proc.csv"
"ts,elapsed_s,cpu_total_s,rss_mb" | Out-File -FilePath $out -Encoding ascii
$sw = [Diagnostics.Stopwatch]::StartNew()
while ($true) {
    try {
        $p = Get-Process sgate -ErrorAction Stop
        $ts = (Get-Date).ToString("o")
        $el = [math]::Round($sw.Elapsed.TotalSeconds, 0)
        $cpu = [math]::Round($p.CPU, 1)
        $rss = [math]::Round($p.WorkingSet64 / 1MB, 1)
        "$ts,$el,$cpu,$rss" | Out-File -FilePath $out -Append -Encoding ascii
    } catch {}
    Start-Sleep -Seconds 10
}
