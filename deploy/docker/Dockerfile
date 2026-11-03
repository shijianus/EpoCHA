# 阶段 5A：单节点 VPS 的生产镜像。
#
# 两个阶段：
#   toolchain  —— 编译 cmd/server 与 cmd/ops（纯 Go、CGO_ENABLED=0、GOTOOLCHAIN=local）
#   runtime    —— `FROM scratch` + 那两把二进制
#
# 为什么运行时是 scratch 而不是 alpine/slim：
#   1. 交付面里"有 shell、有包管理器"不是运维能力，是可执行任意命令的攻击面；本镜像的运维能力
#      就是 `epocha-ops` 的子命令集合，它没有通用 shell，也就没有"进容器里看看"这条路。
#   2. 没有 /bin/sh ⇒ HEALTHCHECK 不能用 `curl`/`wget`（它们不存在）。这里用 `epocha-ops check`，
#      它是真实存在于镜像里、且**只允许**打 /readyz 或 /healthz 的探针命令。
#   3. 服务不发出站请求（无在线调用），因此不需要 CA 证书；把 /etc/ssl 拷进来只是扩大可写面。
#
# 为什么不放 /etc/passwd：用户以**数字 uid** 指定（USER 与 compose 的 `user:`）。
# 把系统用户表拷进 scratch 镜像没有任何解析需求，只是多一份可被读取的文件。
#
# toolchain 阶段可以用命名上下文替换（见 Makefile 的 docker-image 与 docs/DEPLOYMENT.md §3）：
# `--build-context toolchain=<预编译目录>` 会取代下面这段定义，运行时层的内容完全相同。
# 这样"构建机磁盘/网络不足以在容器内下载 Go 模块"时，交付的镜像仍然是同一份配方产物。

ARG BUILDER=golang:1.27.3-alpine
FROM ${BUILDER} AS toolchain

# 构建期不允许隐式切换工具链；显式升级记录在 ADR-020。
ENV GOTOOLCHAIN=local \
    CGO_ENABLED=0 \
    GOFLAGS=-trimpath \
    GOPROXY=https://proxy.golang.org,direct

WORKDIR /src

# 先只拷 go.mod/go.sum：依赖层与源码层分开，改代码不会让依赖下载重来。
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=unknown
ARG COMMIT=none
ARG BUILD_DATE=unknown
# 版本注入与 Makefile 完全同一组 ldflags：镜像里的 `epocha --version` 必须能对上 git 里的 commit，
# 否则备份 manifest 里的 commit 字段就成了无法核对的装饰。
RUN mkdir -p /out && \
    LDFLAGS="-s -w -X github.com/shijianus/EpoCHA/internal/version.Version=${VERSION} \
      -X github.com/shijianus/EpoCHA/internal/version.Commit=${COMMIT} \
      -X github.com/shijianus/EpoCHA/internal/version.BuildDate=${BUILD_DATE}" && \
    go build -trimpath -ldflags "$LDFLAGS" -o /out/epocha ./cmd/server && \
    go build -trimpath -ldflags "$LDFLAGS" -o /out/epocha-ops ./cmd/ops && \
    ls -l /out

FROM scratch AS runtime

COPY --from=toolchain /out/epocha /usr/local/bin/epocha
COPY --from=toolchain /out/epocha-ops /usr/local/bin/epocha-ops

# 65534 = nobody。真实部署要么把数据目录属主改成它，要么用 compose 的 EPOCHA_UID 覆盖，
# 两者都由 docs/DEPLOYMENT.md §2 的前置步骤明确落一遍权限，而不是靠镜像里"看起来非 root"。
USER 65534:65534

WORKDIR /var/lib/epocha

# 公共面在容器内的监听形状：
#   EPOCHA_LISTEN=0.0.0.0:8080      容器内所有网卡（含 eth0）——这是"能被宿主端口映射打到"的前提
#   EPOCHA_ADMIN_LISTEN=127.0.0.1:8081  容器内 loopback——宿主与公网**都**打不到它
# 这两条一起写死在镜像里，是为了让"顺手把管理面发布出去"必须是一次显式的覆盖动作。
ENV EPOCHA_LISTEN=0.0.0.0:8080 \
    EPOCHA_ADMIN_LISTEN=127.0.0.1:8081 \
    EPOCHA_DB=/var/lib/epocha/epocha.db \
    EPOCHA_MASTER_KEY_FILE=/etc/epocha/keys/master \
    EPOCHA_PEPPER_FILE=/etc/epocha/keys/pepper \
    GOMEMLIMIT=56MiB \
    GOMAXPROCS=1

EXPOSE 8080

# 优雅关闭：SIGTERM（Docker 默认停止信号）由 cmd/server 转成 ctx 取消 →
# Shutdown 排空在途请求 → 关闭 DB。stop_grace_period 在 compose 里给 15s，这里不设 shell 包装，
# 因为 scratch 里没有 sh，`ENTRYPOINT ["sh","-c",...]` 会是"看起来能跑其实起不来"的那种错误。
ENTRYPOINT ["/usr/local/bin/epocha"]

# 就绪探针检查连接、schema 与主文件/目录权限，不消费业务状态。
# 文件创建成功不保证后续 WAL 提交成功；磁盘余量和业务错误仍须独立告警。
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/epocha-ops", "check", "--path", "/readyz"]
