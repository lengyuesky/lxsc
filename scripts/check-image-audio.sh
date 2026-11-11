#!/usr/bin/env bash
# 在实际目标架构镜像中解码合成 MP3，依赖与运行层均需真正可用。
set -euo pipefail
cd "$(dirname "$0")/.."
checkImage=${1:?需要镜像名称或 digest}
checkPCM=$(mktemp "${RUNNER_TEMP:-/tmp}/lxsc-pcm-XXXXXX")
checkPackages=$(mktemp "${RUNNER_TEMP:-/tmp}/lxsc-apk-XXXXXX")
trap 'rm -f "$checkPCM" "$checkPackages"' EXIT
docker run --rm -i --network none --read-only --cap-drop ALL --security-opt no-new-privileges --entrypoint ffmpeg "$checkImage" \
  -hide_banner -loglevel error -nostdin -max_alloc 16777216 -threads 1 -filter_threads 1 \
  -protocol_whitelist pipe -format_whitelist mp3,flac,ogg,wav,aac,mov,ape,wv,aiff,au \
  -probesize 262144 -analyzeduration 1000000 -err_detect explode -xerror \
  -i pipe:0 -map 0:a:0 -t 0.05 -ac 1 -ar 8000 -f s16le pipe:1 \
  < internal/testaudio/tone.mp3 > "$checkPCM"
checkBytes=$(wc -c < "$checkPCM")
(( checkBytes >= 512 )) || { echo '镜像未成功解码出真实 PCM 样本。' >&2; exit 1; }
docker run --rm --network none --read-only --entrypoint cat "$checkImage" /usr/share/licenses/lxsc/ALPINE_PACKAGES.txt > "$checkPackages"
grep -q '^P:ffmpeg$' "$checkPackages"
printf '镜像实际解码通过：%s 字节 PCM；FFmpeg 包记录存在。\n' "$checkBytes"
