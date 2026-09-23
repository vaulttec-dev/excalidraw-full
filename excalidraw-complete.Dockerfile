# 前端构建阶段
#
# The frontend is our fork of the official Excalidraw (vaulttec-dev/excalidraw),
# vendored as the `excalidraw` submodule. The fork carries only our own changes
# on top of upstream master, so moving to a newer editor is a merge of upstream
# into the fork — not a rebase of a third party's branch. Upstream pins yarn, so
# the build uses yarn and its lockfile untouched.
FROM --platform=$BUILDPLATFORM node:24 AS frontend-builder
WORKDIR /app
# 复制 excalidraw 子模块
COPY excalidraw/ ./excalidraw/
# 构建前端
# Optional dependencies must not be skipped: without them rollup cannot find
# @rollup/rollup-linux-x64-gnu.
RUN cd excalidraw \
    && yarn --frozen-lockfile --network-timeout 600000 \
    && yarn build:app:docker

# 后端构建阶段
FROM --platform=$BUILDPLATFORM golang:alpine AS backend-builder
RUN apk update && apk add --no-cache git
WORKDIR /app
ARG TARGETOS
ARG TARGETARCH
# 复制 Go 模块文件
COPY go.mod go.sum ./
RUN go mod download
# 复制源代码
COPY . .
# 复制前端构建文件到正确位置，以便 Go embed 可以找到
COPY --from=frontend-builder /app/excalidraw/excalidraw-app/build ./frontend/
# 构建 Go 应用
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w" -o main .

# 最终运行镜像
FROM --platform=$TARGETPLATFORM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
# 复制后端二进制文件（已包含嵌入的前端文件）
COPY --from=backend-builder /app/main .
# 暴露端口
EXPOSE 3002
# /healthz answers once the board list has been read from storage, so an
# instance that cannot reach its bucket is reported unhealthy. The start period
# covers that first read. wget comes with alpine's busybox.
HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:3002/healthz || exit 1
CMD ["./main"]
