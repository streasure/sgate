# 多阶段构建 Dockerfile
# 阶段1: 构建阶段
FROM golang:1.21-alpine AS builder

# 设置工作目录
WORKDIR /app

# 安装构建依赖
RUN apk add --no-cache git ca-certificates tzdata

# 复制 go.mod 和 go.sum
COPY go.mod go.sum ./

# 下载依赖
RUN go mod download

# 复制源代码
COPY . .

# 编译应用程序
# -ldflags 用于减小二进制文件大小并注入版本信息
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-w -s -X main.version=$(git describe --tags --always) -X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -o sgate \
    ./examples/high_concurrency_gateway/main.go

# 阶段2: 运行阶段
FROM alpine:latest

# 安装运行时依赖
RUN apk --no-cache add ca-certificates tzdata

# 创建非 root 用户
RUN addgroup -g 1000 -S sgate && \
    adduser -u 1000 -S sgate -G sgate

# 设置时区
ENV TZ=Asia/Shanghai

# 设置工作目录
WORKDIR /app

# 从构建阶段复制二进制文件
COPY --from=builder /app/sgate .

# 复制配置文件
COPY --from=builder /app/examples/high_concurrency_gateway/config ./config

# 创建日志目录
RUN mkdir -p /app/logs && chown -R sgate:sgate /app

# 切换到非 root 用户
USER sgate

# 暴露端口
# 8080: HTTP API
# 8083: TCP
# 8084: UDP
# 8085: WebSocket
# 9090: Prometheus 指标
EXPOSE 8080 8083 8084 8085 9090

# 健康检查
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/health || exit 1

# 启动命令
CMD ["./sgate"]
