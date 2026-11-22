# API 服务器的镜像(Cloud Run / GKE 共用)。生成的代码已经在仓库里(factory prepare 的产物),这里只编译。
FROM golang:1.27.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY gen ./gen
COPY internal ./internal
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
COPY --from=build /out/server /server
# 部署后确认新版本已经在服务: /healthz 的响应头 X-App-Version
ENV APP_VERSION=${VERSION}
# Cloud Run 通过 PORT 指定端口(默认 8080);认证 AUTH_MODE 必须由部署配置给出
ENV PORT=8080
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/server"]
