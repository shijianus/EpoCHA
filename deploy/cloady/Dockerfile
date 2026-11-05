# Cloady 部署适配镜像。
#
# 与 deploy/docker/Dockerfile 的差别只在这里需要的部分：
#   - 运行时用 alpine 而不是 scratch：Cloady 的 exec/健康检查需要一个可执行的 shell 入口，
#     并且环境密钥要在启动时写成 sites 文件（见 entrypoint.sh）。
#   - 密钥走环境变量（Cloady 的 values + secretKeys 会加密存储），不使用 _FILE；
#     这是平台接入面，不是生产 VPS 的正式文件挂载配方。
#   - 数据目录 /var/lib/epocha 在镜像里由 uid 1000 拥有；若平台挂载卷，卷的属主必须匹配。
#
# 构建上下文由 docs/scripts/cloady-deploy.mjs 冻结成白名单目录，不是整个仓库：
# 生产运行镜像不应包含 cmd/exampleapp、cmd/preview、internal/eval、docs 或评测素材。

ARG BUILDER=golang:1.27.3-alpine
FROM ${BUILDER} AS build

ENV GOTOOLCHAIN=local \
    CGO_ENABLED=0 \
    GOFLAGS=-trimpath \
    GOPROXY=https://proxy.golang.org,direct

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=cloady-2026-11-05
ARG COMMIT=6db855675e7b6ccaca9e78a8719365ba598b9671
ARG BUILD_DATE=2026-11-05T00:00:00Z
RUN mkdir -p /out && \
    LDFLAGS="-s -w -X github.com/shijianus/EpoCHA/internal/version.Version=${VERSION} \
      -X github.com/shijianus/EpoCHA/internal/version.Commit=${COMMIT} \
      -X github.com/shijianus/EpoCHA/internal/version.BuildDate=${BUILD_DATE}" && \
    go build -trimpath -ldflags "$LDFLAGS" -o /out/epocha ./cmd/server && \
    go build -trimpath -ldflags "$LDFLAGS" -o /out/epocha-ops ./cmd/ops

FROM alpine:3.22 AS runtime

# 只用数字 uid；/etc/passwd 不参与运行，也不把 shell 暴露给业务进程以外的东西。
RUN mkdir -p /var/lib/epocha && chown 1000:1000 /var/lib/epocha

COPY --from=build /out/epocha /usr/local/bin/epocha
COPY --from=build /out/epocha-ops /usr/local/bin/epocha-ops
COPY entrypoint.sh /usr/local/bin/epocha-entrypoint
RUN chmod 0755 /usr/local/bin/epocha /usr/local/bin/epocha-ops /usr/local/bin/epocha-entrypoint

USER 1000:1000
WORKDIR /var/lib/epocha

# 公共面只监听容器网卡；管理面固定在容器 loopback，绝不随平台端口发布。
# 密钥不设 _FILE：Cloady 的环境变量由 secretKeys 加密存储。
ENV EPOCHA_LISTEN=0.0.0.0:8080 \
    EPOCHA_ADMIN_LISTEN=127.0.0.1:8081 \
    EPOCHA_DB=/var/lib/epocha/epocha.db \
    GOMEMLIMIT=56MiB \
    GOMAXPROCS=1

EXPOSE 8080

HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/epocha-ops", "check", "--path", "/readyz"]

ENTRYPOINT ["/usr/local/bin/epocha-entrypoint"]
