# SGate 生产环境部署指南

## 目录

1. [环境要求](#环境要求)
2. [配置指南](#配置指南)
3. [TLS 配置](#tls-配置)
4. [Redis 分布式部署](#redis-分布式部署)
5. [监控和告警](#监控和告警)
6. [Kubernetes 部署](#kubernetes-部署)
7. [Docker 部署](#docker-部署)
8. [性能调优](#性能调优)
9. [安全加固](#安全加固)
10. [故障排查](#故障排查)

---

## 环境要求

### 硬件要求

| 资源 | 最低配置 | 推荐配置 | 说明 |
|------|---------|---------|------|
| CPU | 4 核心 | 8+ 核心 | 多核支持启用 |
| 内存 | 4 GB | 16+ GB | 高并发需要更多内存 |
| 网络 | 1 Gbps | 10 Gbps | 低延迟需要高带宽 |
| 磁盘 | 50 GB SSD | 100 GB SSD | 日志和缓存 |

### 软件要求

- **操作系统**: Linux (Ubuntu 20.04+, CentOS 8+), Windows Server 2019+, macOS 12+
- **Go**: 1.21+
- **Redis**: 7.0+ (可选，用于分布式部署)
- **Docker**: 20.10+ (可选)
- **Kubernetes**: 1.24+ (可选)

---

## 配置指南

### 完整配置示例

```yaml
# 服务配置
port: 48080
logLevel: info

# 支持的协议列表
transports:
  - protocol: tcp
    port: 48080
  - protocol: udp
    port: 48081
  - protocol: tcp
    port: 48082
    type: websocket

# 网络配置（性能优化）
network:
  tcpKeepAlive: 5m
  readBufferCapBytes: 65536    # 64KB
  writeBufferCapBytes: 65536   # 64KB
  socketRecvBuffer: 262144     # 256KB
  socketSendBuffer: 262144     # 256KB
  eventLoopCount: 0            # 0=自动检测 CPU 核心数
  reusePort: true              # 端口复用
  tcpNoDelay: true            # 禁用 Nagle 算法

# 工作池配置（性能优化）
workerPool:
  minWorkers: 0               # 0=CPU核心数*4
  maxWorkers: 0                # 0=CPU核心数*16
  queueSize: 5000000           # 500 万队列容量
  queueSizeThreshold: 10000    # 队列阈值

# 速率限制器配置
rateLimiter:
  rate: 1000000
  burst: 2000000
  window: 1s

# 安全配置
security:
  authSecret: "your-production-secret-key-here"
  authRoutes:
    - getConnections
    - broadcast
  enableTLS: true
  certificate: "/etc/sgate/tls/cert.pem"
  privateKey: "/etc/sgate/tls/key.pem"

# Redis 分布式配置
redis:
  enabled: true
  addr: "redis-cluster:6379"
  password: ""
  db: 0
  poolSize: 500
  minIdleConns: 50
  keyPrefix: "sgate"
  connTTL: 10m

# 资源限制配置
resources:
  memoryThreshold: 90.0
  cpuThreshold: 90.0
  enableResourceCircuitBreaker: true
  checkInterval: 5s

# 优雅关闭配置
gracefulShutdown:
  enabled: true
  timeout: 60s
  forceCloseTimeout: 120s
  waitForRequests: true

# 指标监控配置
metrics:
  enabled: true
  port: 9090
```

---

## TLS 配置

### 生成 TLS 证书

#### 使用 Let's Encrypt (生产环境推荐)

```bash
# 安装 certbot
sudo apt-get install certbot

# 生成证书
sudo certbot certonly --standalone -d yourdomain.com \
  --cert-path /etc/sgate/tls/cert.pem \
  --key-path /etc/sgate/tls/key.pem
```

#### 使用自签名证书 (测试环境)

项目会自动生成自签名证书，但生产环境不推荐使用。

### 配置 TLS

```yaml
security:
  enableTLS: true
  certificate: "/etc/sgate/tls/cert.pem"
  privateKey: "/etc/sgate/tls/key.pem"
  minVersion: 12  # TLS 1.2
```

### TLS 连接示例

```go
// 使用 TLS 连接
conn, err := tls.Dial("tcp", "localhost:48080", &tls.Config{
    ServerName: "yourdomain.com",
    MinVersion: tls.VersionTLS12,
})
```

---

## Redis 分布式部署

### Redis 配置

```yaml
redis:
  enabled: true
  addr: "redis-cluster:6379"
  password: "your-redis-password"
  db: 0
  poolSize: 500
  minIdleConns: 50
  keyPrefix: "sgate"
  connTTL: 10m
```

### Redis Cluster 配置

```yaml
redis:
  enabled: true
  addr: "redis-1:6379,redis-2:6379,redis-3:6379"
  poolSize: 500
  keyPrefix: "sgate"
```

### 连接状态同步

启用 Redis 后，连接状态会在多个实例间同步：

- 用户登录时状态注册到 Redis
- 心跳更新到 Redis
- 踢人操作同步到所有实例
- 实例故障时连接自动迁移

---

## 监控和告警

### Prometheus 指标

项目导出以下指标到 `/metrics` 端点：

#### 连接指标
- `sgate_connections_total` - 总连接数
- `sgate_connections_active` - 活跃连接数
- `sgate_connections_by_protocol` - 按协议的连接数

#### 消息指标
- `sgate_messages_total` - 总消息数
- `sgate_messages_failed` - 失败消息数
- `sgate_bytes_sent_total` - 发送字节数
- `sgate_bytes_received_total` - 接收字节数

#### 性能指标
- `sgate_request_duration_seconds` - 请求延迟分布
- `sgate_qps` - 当前 QPS

#### 系统指标
- `sgate_goroutines` - Goroutine 数量
- `sgate_memory_bytes` - 内存使用
- `sgate_cpu_percent` - CPU 使用率

### Grafana 仪表盘

创建 `grafana-dashboard.json`:

```json
{
  "dashboard": {
    "title": "SGate Gateway",
    "panels": [
      {
        "title": "QPS",
        "targets": [
          {"expr": "sgate_qps"}
        ]
      },
      {
        "title": "连接数",
        "targets": [
          {"expr": "sgate_connections_active"}
        ]
      },
      {
        "title": "延迟 P99",
        "targets": [
          {"expr": "histogram_quantile(0.99, sgate_request_duration_seconds)"}
        ]
      }
    ]
  }
}
```

### 告警规则

```yaml
groups:
- name: sgate
  rules:
  - alert: HighQPSDrop
    expr: sgate_qps < 1000
    for: 5m
    labels:
      severity: critical
    annotations:
      summary: "QPS 显著下降"
      
  - alert: HighFailureRate
    expr: rate(sgate_messages_failed[5m]) > 0.01
    for: 5m
    labels:
      severity: warning
    annotations:
      summary: "失败率过高"
```

---

## Kubernetes 部署

### 1. 创建 Namespace

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: sgate
```

### 2. 创建 ConfigMap

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: sgate-config
  namespace: sgate
data:
  config.yaml: |
    port: 48080
    logLevel: info
    redis:
      enabled: true
      addr: "redis-cluster:6379"
    metrics:
      enabled: true
      port: 9090
```

### 3. 创建 Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sgate
  namespace: sgate
spec:
  replicas: 3
  selector:
    matchLabels:
      app: sgate
  template:
    metadata:
      labels:
        app: sgate
    spec:
      containers:
      - name: sgate
        image: sgate:latest
        ports:
        - containerPort: 48080
          name: tcp
        - containerPort: 48081
          name: udp
        - containerPort: 48082
          name: websocket
        - containerPort: 9090
          name: metrics
        volumeMounts:
        - name: config
          mountPath: /etc/sgate
        resources:
          requests:
            memory: "512Mi"
            cpu: "500m"
          limits:
            memory: "2Gi"
            cpu: "2000m"
        livenessProbe:
          httpGet:
            path: /health
            port: 9090
          initialDelaySeconds: 10
          periodSeconds: 30
        readinessProbe:
          httpGet:
            path: /ready
            port: 9090
          initialDelaySeconds: 5
          periodSeconds: 10
      volumes:
      - name: config
        configMap:
          name: sgate-config
```

### 4. 创建 Service

```yaml
apiVersion: v1
kind: Service
metadata:
  name: sgate
  namespace: sgate
spec:
  type: LoadBalancer
  selector:
    app: sgate
  ports:
  - port: 48080
    targetPort: 48080
    protocol: TCP
    name: tcp
  - port: 48081
    targetPort: 48081
    protocol: UDP
    name: udp
  - port: 48082
    targetPort: 48082
    protocol: TCP
    name: websocket
```

### 5. 部署

```bash
kubectl apply -f k8s/namespace.yaml
kubectl apply -f k8s/configmap.yaml
kubectl apply -f k8s/deployment.yaml
kubectl apply -f k8s/service.yaml
```

---

## Docker 部署

### 1. 构建镜像

```bash
docker build -t sgate:latest .
```

### 2. 创建配置文件

```bash
mkdir -p /data/sgate/config
cp config/config.yaml /data/sgate/config/
```

### 3. 运行容器

```bash
docker run -d \
  --name sgate \
  -p 48080:48080 \
  -p 48081:48081 \
  -p 48082:48082 \
  -p 9090:9090 \
  -v /data/sgate/config:/etc/sgate \
  -v /data/sgate/logs:/var/log/sgate \
  -e SGATE_CONFIG=/etc/sgate/config.yaml \
  --restart unless-stopped \
  --ulimit nofile=65536:65536 \
  sgate:latest
```

### 4. Docker Compose

```yaml
version: '3.8'

services:
  sgate:
    image: sgate:latest
    ports:
      - "48080:48080"
      - "48081:48081"
      - "48082:48082"
      - "9090:9090"
    volumes:
      - ./config:/etc/sgate
      - ./logs:/var/log/sgate
    environment:
      - SGATE_CONFIG=/etc/sgate/config.yaml
      - SGATE_LOG_LEVEL=info
    ulimits:
      nofile:
        soft: 65536
        hard: 65536
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:9090/health"]
      interval: 30s
      timeout: 10s
      retries: 3

  redis:
    image: redis:7-alpine
    ports:
      - "6379:6379"
    volumes:
      - redis-data:/data
    restart: unless-stopped

volumes:
  redis-data:
```

---

## 性能调优

### 1. 操作系统参数

```bash
# /etc/sysctl.conf

# 网络参数
net.core.somaxconn = 65535
net.core.netdev_max_backlog = 65535
net.ipv4.tcp_max_syn_backlog = 65535
net.ipv4.ip_local_port_range = 1024 65535

# 内存参数
vm.max_map_count = 262144
vm.overcommit_memory = 1

# 文件描述符
fs.file-max = 2097152
```

### 2. 应用参数

```yaml
network:
  readBufferCapBytes: 131072    # 128KB
  writeBufferCapBytes: 131072   # 128KB
  socketRecvBuffer: 524288      # 512KB
  socketSendBuffer: 524288      # 512KB

workerPool:
  minWorkers: 0                  # CPU*4
  maxWorkers: 0                  # CPU*16
  queueSize: 10000000          # 1000 万
```

### 3. 压测验证

```bash
# 单机压测
./loadtest.exe

# 分布式压测（多台机器同时压测）
./loadtest.exe -addr=<server-ip>
```

---

## 安全加固

### 1. 网络安全

- [ ] 使用防火墙限制访问
- [ ] 启用 TLS 加密
- [ ] 配置 IP 白名单/黑名单
- [ ] 使用 VPC/Private Network

### 2. 应用安全

- [ ] 配置强 JWT 密钥
- [ ] 启用消息校验
- [ ] 配置速率限制
- [ ] 定期更新依赖

### 3. 监控告警

- [ ] 配置 QPS 告警
- [ ] 配置失败率告警
- [ ] 配置资源使用告警
- [ ] 配置连接数告警

### 4. 备份恢复

- [ ] 定期备份配置
- [ ] 测试恢复流程
- [ ] 记录恢复时间

---

## 故障排查

### 1. 连接问题

```bash
# 检查端口占用
netstat -tulpn | grep 48080

# 检查连接数
ss -s

# 检查防火墙
iptables -L -n
```

### 2. 性能问题

```bash
# 检查 CPU 使用
top -H

# 检查内存使用
free -h

# 检查网络带宽
sar -n DEV 1

# 检查 goroutine 泄漏
curl http://localhost:9090/metrics | grep goroutines
```

### 3. 日志分析

```bash
# 查看错误日志
grep -i error /var/log/sgate/*.log

# 查看连接日志
grep "connection" /var/log/sgate/*.log

# 实时查看日志
tail -f /var/log/sgate/*.log
```

### 4. 健康检查

```bash
# 检查健康状态
curl http://localhost:9090/health

# 检查就绪状态
curl http://localhost:9090/ready

# 检查存活状态
curl http://localhost:9090/live

# 检查指标
curl http://localhost:9090/metrics
```

---

## 总结

本指南涵盖了 SGate 网关服务在生产环境部署的所有关键方面：

- ✅ 环境配置
- ✅ TLS 安全配置
- ✅ Redis 分布式部署
- ✅ 监控和告警
- ✅ Kubernetes/Docker 部署
- ✅ 性能调优
- ✅ 安全加固
- ✅ 故障排查

遵循本指南可以确保 SGate 网关服务在生产环境中的稳定、安全、高性能运行。

如有问题，请参考 [USAGE_GUIDE.md](./USAGE_GUIDE.md) 或提交 Issue。
