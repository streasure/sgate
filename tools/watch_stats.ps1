# churn30 期间采样 sgate 系统指标 -> CSV（每 10s 一行，含进程 CPU/RSS）
# 用法: Start-Process powershell -ArgumentList "-NoProfile","-ExecutionPolicy","Bypass","-File","tools\watch_stats.ps1" -WindowStyle Hidden
$out = "E:\sgate\logs\churn30_monitor.csv"
"ts,elapsed_s,conns,goroutines,alloc_mb,sys_mb,gc_count,msgps,cpu_pct,mem_pct,dropped_total,forwarded,rss_mb,proc_cpu_s" | Out-File -FilePath $out -Encoding ascii
$sw = [Diagnostics.Stopwatch]::StartNew()
while ($true) {
    $ts = (Get-Date).ToString("o")
    $el = [math]::Round($sw.Elapsed.TotalSeconds, 0)
    $conns = ""; $gr = ""; $alloc = ""; $sys = ""; $gcc = ""; $msgps = ""
    $cpu = ""; $mpc = ""; $drop = ""; $fwd = ""
    try {
        $h = Invoke-RestMethod -Uri "http://127.0.0.1:8081/health" -TimeoutSec 5
        $conns = $h.metrics.connections
        $gr = $h.metrics.goroutines
        $alloc = $h.metrics.memory_alloc_mb
        $sys = $h.metrics.memory_sys_mb
        $gcc = $h.metrics.gc_count
        $msgps = $h.metrics.messages_per_sec
    } catch {}
    try {
        $s = Invoke-RestMethod -Uri "http://127.0.0.1:8081/stats" -TimeoutSec 5
        $cpu = $s.cpuPercent
        $mpc = $s.memPercent
        $drop = $s.droppedTotal
        $fwd = $s.forwarded
    } catch {}
    $rss = ""; $pcpu = ""
    try { $p = Get-Process sgate -ErrorAction Stop; $rss = [math]::Round($p.WorkingSet64 / 1MB, 1); $pcpu = [math]::Round($p.CPU, 1) } catch {}
    "$ts,$el,$conns,$gr,$alloc,$sys,$gcc,$msgps,$cpu,$mpc,$drop,$fwd,$rss,$pcpu" | Out-File -FilePath $out -Append -Encoding ascii
    Start-Sleep -Seconds 10
}
