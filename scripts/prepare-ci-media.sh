#!/usr/bin/env bash
# 仅在 GitHub Ubuntu runner 中配置官方 APT 源；本地已有 FFmpeg 时直接复用。
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ "${GITHUB_ACTIONS:-}" == "true" ]]; then
  ciRelease=$(awk -F= '$1 == "VERSION_CODENAME" { gsub(/"/, "", $2); print $2 }' /etc/os-release)
  [[ "$ciRelease" == "noble" ]] || { echo 'CI 媒体准备仅支持 Ubuntu 24.04。' >&2; exit 1; }
  ciArch=$(dpkg --print-architecture)
  case "$ciArch" in
    amd64) ciArchive=https://archive.ubuntu.com/ubuntu; ciSecurity=https://security.ubuntu.com/ubuntu ;;
    arm64) ciArchive=https://ports.ubuntu.com/ubuntu-ports; ciSecurity=$ciArchive ;;
    *) echo '不支持的 CI 架构。' >&2; exit 1 ;;
  esac
  ciSources=$(mktemp "${RUNNER_TEMP:-/tmp}/lxsc-ubuntu-XXXXXX.sources")
  ciOptions=$(mktemp "${RUNNER_TEMP:-/tmp}/lxsc-apt-XXXXXX.conf")
  ciHosts=$(mktemp "${RUNNER_TEMP:-/tmp}/lxsc-ubuntu-XXXXXX.hosts")
  trap 'rm -f "$ciSources" "$ciOptions" "$ciHosts"' EXIT
  cat > "$ciSources" <<EOF
Types: deb
URIs: $ciArchive
Suites: noble noble-updates noble-backports
Components: main restricted universe multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg

Types: deb
URIs: $ciSecurity
Suites: noble-security
Components: main restricted universe multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
EOF
  cat > "$ciOptions" <<'EOF'
Acquire::Retries "1";
Acquire::http::Timeout "15";
Acquire::https::Timeout "15";
Acquire::ForceIPv4 "true";
APT::Update::Error-Mode "any";
DPkg::Lock::Timeout "30";
EOF
  # runner 的镜像列表曾持续解析失败；直接使用同发行版的签名官方源。
  # 兼容仍采用 sources.list 的 runner，避免旧镜像条目继续参与下载。
  for ciLegacy in /etc/apt/sources.list /etc/apt/sources.list.d/*.list; do
    if [[ -f "$ciLegacy" ]]; then
      sudo sed -i -E '/^[[:space:]]*deb(-src)?[[:space:]].*(archive\.ubuntu\.com\/ubuntu|security\.ubuntu\.com\/ubuntu|ports\.ubuntu\.com\/ubuntu-ports|mirror\+file:\/etc\/apt\/apt-mirrors\.txt)/d' "$ciLegacy"
    fi
  done
  sudo install -m 644 "$ciSources" /etc/apt/sources.list.d/ubuntu.sources
  sudo install -m 644 "$ciOptions" /etc/apt/apt.conf.d/80-lxsc-ci-network
  # runner 的系统 DNS 对 Ubuntu 域名持续失败，通过 HTTPS DNS 获取临时地址。
  ciArchiveHost=${ciArchive#https://}; ciArchiveHost=${ciArchiveHost%%/*}
  ciSecurityHost=${ciSecurity#https://}; ciSecurityHost=${ciSecurityHost%%/*}
  node scripts/ci-ubuntu-hosts.mjs "$ciArchiveHost" "$ciSecurityHost" > "$ciHosts"
  sudo tee -a /etc/hosts < "$ciHosts" >/dev/null
  printf 'CI 架构=%s，CPU=%s\n' "$ciArch" "$(nproc)"
  awk '/MemTotal/ { printf "CI 内存=%.0f MiB\n", $2 / 1024 }' /proc/meminfo
fi

if command -v ffmpeg >/dev/null 2>&1 && ffmpeg -hide_banner -version >/dev/null 2>&1; then
  echo '复用已有 FFmpeg。'
  exit 0
fi
[[ "${GITHUB_ACTIONS:-}" == "true" ]] || { echo '请先安装可运行的 FFmpeg。' >&2; exit 1; }

# 限定每阶段等待时间；索引失败即停止，不能继续逐个等待旧索引中的包。
timeout --kill-after=10s 90s sudo env DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a apt-get update
timeout --kill-after=10s 120s sudo env DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a apt-get install -y --no-install-recommends ffmpeg
ffmpeg -hide_banner -version
