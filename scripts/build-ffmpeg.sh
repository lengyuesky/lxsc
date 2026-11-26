#!/bin/sh
# 编译仅供音频内容验证使用的静态 FFmpeg：只含管道输入、白名单容器的解复用器、
# 常见音频解码器与 PCM 输出，不含视频、图形、网络、硬件加速或 GPL/nonfree 组件（LGPL-2.1-or-later）。
#
# 组件必须覆盖 internal/httpguard/audio.go 的 decodeAudio 参数：
#   -protocol_whitelist pipe、-format_whitelist（与下方 demuxers 一致）、-ac/-ar 重采样、-t 截断、-f s16le 输出。
# 变更组件后须用 scripts/check-image-audio.sh 在实际镜像中重新验证。
set -eu
src=${1:?需要 FFmpeg 源码目录}
out=${2:?需要输出目录}

demuxers=mp3,flac,ogg,wav,aac,mov,ape,wv,aiff,au
decoders='mp1,mp1float,mp2,mp2float,mp3,mp3float,flac,alac,aac,aac_fixed,aac_latm,vorbis,opus,ape,wavpack,ac3,ac3_fixed,eac3,pcm_*,adpcm_ms,adpcm_ima_wav,gsm_ms'
parsers=mpegaudio,flac,aac,aac_latm,vorbis,opus,ac3

set -- \
  --prefix=/usr/local --enable-static --disable-shared --extra-ldflags=-static \
  --disable-autodetect --disable-everything --disable-doc --disable-debug --disable-network \
  --disable-ffplay --disable-ffprobe --disable-avdevice --disable-swscale --disable-asm \
  --enable-swresample \
  --enable-protocol=pipe \
  --enable-demuxer="$demuxers" \
  --enable-decoder="$decoders" \
  --enable-parser="$parsers" \
  --enable-muxer=pcm_s16le --enable-encoder=pcm_s16le \
  --enable-filter=aresample,aformat,anull,atrim

cd "$src"
./configure "$@"
make -j"$(nproc)" ffmpeg
strip ffmpeg
mkdir -p "$out/legal"
cp ffmpeg "$out/ffmpeg"
{
  echo "FFmpeg $(./ffmpeg -version | sed -n '1s/^ffmpeg version \([^ ]*\).*/\1/p')"
  echo "source: ${FFMPEG_SOURCE_URL:-unknown}"
  echo "sha256: ${FFMPEG_SHA256:-unknown}"
  echo "license: LGPL-2.1-or-later（未启用 --enable-gpl / --enable-nonfree）"
  echo "build script: scripts/build-ffmpeg.sh"
  echo "configure:"
  printf '  %s\n' "$@"
} > "$out/legal/BUILD.txt"
