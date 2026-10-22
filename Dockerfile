# ---- 构建阶段：编译 Go 静态二进制 ----
FROM golang:1.27-alpine AS build

# 使用国内模块代理（proxy.golang.org 在本网络不可达）
ENV GOPROXY=https://goproxy.cn,direct

WORKDIR /src

# 先下载依赖，源码变更时不破坏依赖缓存层
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO_ENABLED=0 静态编译，兼容 alpine(musl)，运行层无需 libc 兼容问题
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/server ./cmd/server

# ---- 运行阶段 ----
FROM alpine:3.20

RUN apk add --no-cache ca-certificates wget

COPY --from=build /out/server /usr/local/bin/server

# 容器内监听 8081，与宿主机端口映射保持一致（HTTP_ADDR=:8081）
EXPOSE 8081

# 优雅停机由 compose 发 SIGTERM，直接 exec 形式
CMD ["server"]
