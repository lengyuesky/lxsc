# 阶段 1：打包 JS 桥接（prelude + musicSdk）
FROM oven/bun:1 AS js
WORKDIR /src/js-bridge
COPY js-bridge/package.json js-bridge/bun.lock* ./
RUN bun install --frozen-lockfile || bun install
COPY js-bridge/ ./
RUN mkdir -p /src/internal/assets/js && bun run build

# 阶段 2：编译 Go
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=js /src/internal/assets/js/ ./internal/assets/js/
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /lxsc ./cmd/lxsc

# 阶段 3：运行镜像
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 1000 lxsc
ENV LXSC_DATA_DIR=/data LXSC_LISTEN=:8080 TZ=Asia/Shanghai
COPY --from=build /lxsc /usr/local/bin/lxsc
RUN mkdir -p /data && chown lxsc /data
USER lxsc
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["lxsc"]
