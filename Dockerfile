# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# 构建阶段
#
# 基镜像选 golang:1.24-alpine：本项目零第三方依赖，go.mod 里没有任何 require，
# 因此构建阶段极快，不需要额外的模块缓存优化。
# ---------------------------------------------------------------------------
FROM golang:1.24-alpine AS build
WORKDIR /src

# 依赖清单先拷（层缓存友好：改源码不会重跑 mod 下载）。
# 本项目零依赖，这一步实际上只是建立 go.mod 的层。
COPY go.mod ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 → 纯静态二进制，可以跑在 alpine 上而不需要 glibc。
# -trimpath 去掉本机绝对路径（可复现构建）；-s -w 去符号表（体积减半）。
ARG APP_VERSION=0.2.5
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags "-s -w -X trae2api/internal/server.Version=${APP_VERSION}" \
      -o /out/trae2api ./cmd/trae2api

# ---------------------------------------------------------------------------
# 运行阶段
#
# 基镜像选 alpine 而不是 scratch/distroless，理由：
#   1) 需要出站 HTTPS（调 api.trae.cn）→ 必须有根证书；
#   2) 签到定时任务按**本地时间**计算 → 必须有 tzdata，否则 TZ 设置无效；
#   3) 保留 shell 便于排障（本项目是自托管本地工具，可调试性优先于极致精简）。
#
# alpine 用的是 busybox 方言：建用户/建组是 adduser/addgroup（不是 useradd/groupadd）。
# alpine 基础镜像里没有 10001 号用户，用它可避开与任何内置用户撞号。
# ---------------------------------------------------------------------------
FROM alpine:3.20

# ca-certificates：出站 HTTPS 必需，缺了会 x509: certificate signed by unknown authority
# tzdata        ：签到定时任务按本地时间计算，缺了 TZ 环境变量不生效（会按 UTC 跑）
RUN apk add --no-cache ca-certificates tzdata

ARG APP_UID=10001
ARG APP_GID=10001

# 建用户/建组：**容忍撞号**（见 docker skill 1.3.3）。
#
# 为什么不能直接 addgroup/adduser：APP_UID/APP_GID 可能是 --build-arg 从宿主传进来的
# （CI 或部署流程常把 `id -u` 直接传进来，而 Linux 宿主的 id -u 通常是 1000）。
# 那种情况下开发机永远构建成功（用默认 10001），**只在用户服务器上失败**，
# 报错还停在 [2/N] 这一行，看不出根因。
#
# 所以：号空闲就建，已被占用就**复用现有身份**（不报错）。
# 全程用数字 ID（不写用户名），因为复用场景下那个用户不叫 trae2api。
RUN set -eux; \
    if ! grep -qE ":${APP_GID}:" /etc/group; then \
      addgroup -g "${APP_GID}" trae2api; \
    fi; \
    if ! grep -qE ":${APP_UID}:" /etc/passwd; then \
      GROUP_NAME="$(awk -F: -v gid="${APP_GID}" '$3 == gid { print $1 }' /etc/group)"; \
      adduser -u "${APP_UID}" -D -G "${GROUP_NAME}" -h /app -s /sbin/nologin trae2api; \
    fi; \
    mkdir -p /app/data /trae; \
    chown -R "${APP_UID}:${APP_GID}" /app /trae

WORKDIR /app

# 数据目录与挂载点已在上面那条 RUN 里建好并 chown 过了。
# 这里只拷产物与配置 —— 用**数字 ID** 而不是用户名：
# 撞号复用的场景下那个用户不叫 trae2api，写死名字会失败。
COPY --from=build --chown=${APP_UID}:${APP_GID} /out/trae2api /usr/local/bin/trae2api

# 容器专用默认配置：listen 必须是 0.0.0.0（127.0.0.1 在容器里无法被外部访问），
# 且账号必须显式给 storage_path（容器里没有 %APPDATA%，自动探测必然失败）。
COPY --chown=${APP_UID}:${APP_GID} docker/config.docker.json /app/config.json

# 构建期自检：权限配错要在**构建时**就炸，而不是等容器跑起来报 EACCES。
#
# 注意每条检查各自能证明什么（别自欺）：
#   - test -x：二进制确实装上了且有执行位（Windows 宿主上 COPY 丢执行位是常见坑）。
#   - stat 属主断言：/app/data 的属主确实是 ${APP_UID}:${APP_GID} —— **这条才是关键**。
#     命名卷首次创建时会继承挂载点的属主，属主错了非 root 进程就写不进去。
#   - 刻意**不**写 `test -w /app/data`：这里还是 root，root 对任何目录都可写，
#     那条检查恒真、证明不了任何事。（运行期可写性由程序启动时自己探测并告警。）
RUN test -x /usr/local/bin/trae2api \
 && test "$(stat -c '%u:%g' /app/data)" = "${APP_UID}:${APP_GID}" \
 && test "$(stat -c '%u:%g' /trae)" = "${APP_UID}:${APP_GID}"

USER ${APP_UID}:${APP_GID}

# 容器内端口（对外映射由 compose 决定）
EXPOSE 7870

# 健康检查用 busybox wget（alpine 自带），走容器内端口
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD wget -qO- http://127.0.0.1:7870/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/trae2api"]
CMD ["serve", "-config", "/app/config.json"]
