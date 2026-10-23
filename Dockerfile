# chatgpt2api 2.0 — Go + Gin 单二进制
#
# 三阶段：
#   web-build  构建 Next.js 静态导出（产物进 web_dist）
#   go-build   编译 Go 二进制，把 web_dist 一起 embed
#   runtime    distroless 静态镜像（无 shell、无包管理器）

ARG BUILDPLATFORM
ARG TARGETPLATFORM
ARG TARGETARCH

# ---------- 前端 ----------
FROM --platform=$BUILDPLATFORM node:22-alpine AS web-build

WORKDIR /app/web

COPY web/package.json web/bun.lock ./
RUN npm install

COPY VERSION /app/VERSION
COPY CHANGELOG.md /app/CHANGELOG.md
COPY web ./
RUN NEXT_PUBLIC_APP_VERSION="$(cat /app/VERSION)" npm run build

# ---------- 后端 ----------
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS go-build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# 先只拷依赖清单，让模块下载层可缓存。
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    GOPROXY=https://proxy.golang.org,direct go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY VERSION ./VERSION

# 前端产物落到 embed 目录（覆盖 .gitkeep 占位）。
COPY --from=web-build /app/web/out ./internal/web/web_dist

# CGO_ENABLED=0：纯静态二进制，能跑在 distroless/static 上。
# -trimpath -ldflags "-s -w"：去掉构建路径与符号表，镜像更小。
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/chatgpt2api ./cmd/server

# ---------- 运行时 ----------
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=go-build /out/chatgpt2api /app/chatgpt2api
COPY --from=go-build /src/VERSION /app/VERSION

# 数据目录与配置文件由 compose 挂载；镜像里只放 VERSION。
# distroless 的 nonroot 用户 uid=65532，挂载目录需要可写。
USER nonroot:nonroot

EXPOSE 80

ENTRYPOINT ["/app/chatgpt2api"]
CMD ["--addr", ":80"]
