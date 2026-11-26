#!/usr/bin/env bash
# 在实际目标架构镜像中解码合成 MP3 与 FLAC，最小 FFmpeg 与运行层均需真正可用。
set -euo pipefail
cd "$(dirname "$0")/.."
checkImage=${1:?需要镜像名称或 digest}
checkPCM=$(mktemp "${RUNNER_TEMP:-/tmp}/lxsc-pcm-XXXXXX")
checkPackages=$(mktemp "${RUNNER_TEMP:-/tmp}/lxsc-apk-XXXXXX")
trap 'rm -f "$checkPCM" "$checkPackages"' EXIT
checkDecode() {
  docker run --rm -i --network none --read-only --cap-drop ALL --security-opt no-new-privileges --entrypoint ffmpeg "$checkImage" \
    -hide_banner -loglevel error -nostdin -max_alloc 16777216 -threads 1 -filter_threads 1 \
    -protocol_whitelist pipe -format_whitelist mp3,flac,ogg,wav,aac,mov,ape,wv,aiff,au \
    -probesize 262144 -analyzeduration 1000000 -err_detect explode -xerror \
    -i pipe:0 -map 0:a:0 -t 0.05 -ac 1 -ar 8000 -f s16le pipe:1 \
    < "$1" > "$checkPCM"
  checkBytes=$(( $(wc -c < "$checkPCM") ))
  (( checkBytes >= 512 )) || { echo "镜像未能从 $1 解码出真实 PCM 样本。" >&2; exit 1; }
}
checkDecode internal/testaudio/tone.mp3
checkDecode internal/testaudio/tone.flac
docker run --rm --network none --read-only --entrypoint cat "$checkImage" /usr/share/licenses/lxsc/ALPINE_PACKAGES.txt > "$checkPackages"
# 运行层只带最小 FFmpeg：不能再装回 Alpine 完整 ffmpeg 及其视频依赖，且须保留构建与许可记录。
if grep -q '^P:ffmpeg$' "$checkPackages"; then echo '运行层不应安装 Alpine 完整 ffmpeg。' >&2; exit 1; fi
docker run --rm --network none --read-only --entrypoint cat "$checkImage" /usr/share/licenses/lxsc/ffmpeg/BUILD.txt | grep -q 'LGPL-2.1-or-later'
printf '镜像实际解码通过（MP3、FLAC）：%s 字节 PCM；最小 FFmpeg 构建记录存在。\n' "$checkBytes"
