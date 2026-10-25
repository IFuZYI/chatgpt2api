# chatgpt2api 2.0 — Go + Gin 单二进制
#
# 三阶段构建：
#   web-build  Next.js 静态导出（产物进 internal/web/web_dist）
#   go-build   编译 Go 二进制，把前端产物 embed 进二进制
#   runtime    distroless 静态镜像（无 shell、无包管理器、非 root）
#
# 镜像不含前端源码，也不含 data/：前端在构建期 embed，数据在运行期挂载。

ARG BUILDPLATFORM
ARG TARGETPLATFORM

# ---------- 前端 ----------
FROM --platform=$BUILDPLATFORM node:22-alpine AS web-build

WORKDIR /app/web

# 只用 package-lock.json：仓库里同时有 bun.lock，但构建统一走 npm，
# 锁两份会让「本地 bun 装、CI npm 装」得到不同的依赖树。
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund

COPY VERSION /app/VERSION
COPY CHANGELOG.md /app/CHANGELOG.md
COPY web ./
# NEXT_PUBLIC_APP_VERSION 在构建期注入（前端读它显示版本号）。
RUN NEXT_PUBLIC_APP_VERSION="$(cat /app/VERSION)" npm run build

# ---------- 后端 ----------
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS go-build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# 先只拷依赖清单，让 go mod download 这层可缓存。
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    GOPROXY=https://proxy.golang.org,direct go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY VERSION ./VERSION

# 前端产物落到 embed 目录（覆盖 .gitkeep 占位）。
COPY --from=web-build /app/web/out ./internal/web/web_dist

# CGO_ENABLED=0    纯静态二进制，能跑在 distroless/static 上
# -trimpath        去掉构建机路径
# -ldflags -s -w   去符号表，二进制更小
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/chatgpt2api ./cmd/server

# ---------- 运行时 ----------
# 这个 stage 必须命名（AS runtime）：CI 用 `target: runtime` 只构建到运行时层，
# 也便于 `docker build --target runtime` 单独验证。历史上最终 stage 未命名，
# 而 workflow 写着 `target: app` —— 那个 stage 不存在，CI 从未成功构建过。
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

WORKDIR /app

COPY --from=go-build /out/chatgpt2api /app/chatgpt2api
COPY --from=go-build /src/VERSION /app/VERSION

# 监听 8080 而不是 80：
# distroless 的 nonroot（uid 65532）无法绑定 <1024 的端口
# （需要 CAP_NET_BIND_SERVICE，本镜像不授予）。用 :80 会直接
# `listen tcp :80: bind: permission denied` 起不来。
# 对外端口由 compose 的 ports 映射决定，不影响用户看到的端口。
USER nonroot:nonroot

EXPOSE 8080

# data/ 与 config.json 由 compose 挂载，且必须对 uid 65532 可写。
# 首次启动若目录为空，服务会自行创建 config.json 与数据文件。
ENTRYPOINT ["/app/chatgpt2api"]
CMD ["--addr", ":8080"]
