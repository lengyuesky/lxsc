# 阶段 1：在构建机架构打包 JS，禁止安装失败时绕过锁文件
FROM --platform=$BUILDPLATFORM oven/bun:1.3.14 AS js
WORKDIR /src/js-bridge
COPY js-bridge/package.json js-bridge/bun.lock ./
RUN bun install --frozen-lockfile
COPY js-bridge/ ./
RUN mkdir -p /src/internal/assets/js && bun run build

# 阶段 2：无 CGO 的 Go 交叉编译，不使用 QEMU 编译工具链
FROM --platform=$BUILDPLATFORM golang:1.27.0-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,id=lxsc-go-mod go mod download
COPY . .
COPY --from=js /src/internal/assets/js/ ./internal/assets/js/
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod,id=lxsc-go-mod \
    --mount=type=cache,target=/root/.cache/go-build,id=lxsc-go-build-${TARGETARCH},sharing=locked \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /lxsc ./cmd/lxsc

# 阶段 3：只编译音频验证用到的 FFmpeg 组件（静态链接、LGPL），
# 不安装 Alpine 完整 ffmpeg 及其上百个视频、图形、声卡依赖。源码包校验官方发布的 SHA-256。
FROM alpine:3.24 AS ffmpeg
ARG FFMPEG_VERSION=8.1.3
ARG FFMPEG_SHA256=7138d28c96d9d3e3af4ee3d8cad72741f8ffb40da90c1112235dea3ecd3178a3
ENV FFMPEG_SOURCE_URL=https://ffmpeg.org/releases/ffmpeg-${FFMPEG_VERSION}.tar.xz FFMPEG_SHA256=${FFMPEG_SHA256}
RUN apk add --no-cache build-base xz \
    && wget -q -O /tmp/ffmpeg.tar.xz "$FFMPEG_SOURCE_URL" \
    && echo "$FFMPEG_SHA256  /tmp/ffmpeg.tar.xz" | sha256sum -c - \
    && mkdir /tmp/ffmpeg && tar -xJf /tmp/ffmpeg.tar.xz -C /tmp/ffmpeg --strip-components=1
COPY scripts/build-ffmpeg.sh /tmp/build-ffmpeg.sh
RUN sh /tmp/build-ffmpeg.sh /tmp/ffmpeg /out

# 原生构建时运行层无需 QEMU；3.24 处于常规支持期。
FROM alpine:3.24 AS runtime
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 1000 lxsc
COPY --from=ffmpeg /out/ffmpeg /usr/local/bin/ffmpeg
ENV LXSC_DATA_DIR=/data LXSC_LISTEN=:8080 TZ=Asia/Shanghai
LABEL org.opencontainers.image.source="https://github.com/lengyuesky/lxsc"
COPY LICENSE NOTICE THIRD_PARTY_NOTICES.md /usr/share/licenses/lxsc/
COPY licenses/ /usr/share/licenses/lxsc/licenses/
COPY js-bridge/vendor/PATCHES.md /usr/share/licenses/lxsc/js-bridge/vendor/PATCHES.md
COPY --from=ffmpeg /out/legal/ /usr/share/licenses/lxsc/ffmpeg/
# 保存 APK 原始记录，其中 P/V/L/o/c 字段分别给出名称、版本、许可、源码包和配方提交。
RUN cp /lib/apk/db/installed /usr/share/licenses/lxsc/ALPINE_PACKAGES.txt \
    && chmod -R a+rX /usr/share/licenses/lxsc \
    && mkdir -p /data && chown lxsc /data
USER lxsc
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=120s --retries=3 CMD ["lxsc", "-healthcheck"]
ENTRYPOINT ["lxsc"]

# CI 使用经过竞态、JS 产物和许可门禁的原生编译二进制，复用同一运行层。
FROM runtime AS ci
ARG TARGETARCH
COPY .ci/bin/${TARGETARCH}/lxsc /usr/local/bin/lxsc

# 默认仍支持从源码构建，不要求调用方预先准备二进制。
FROM runtime AS final
COPY --from=build /lxsc /usr/local/bin/lxsc
