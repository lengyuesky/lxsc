# lxsc

支持 **洛雪音乐自定义音源脚本** 的 Subsonic / OpenSubsonic 音乐服务端。用音流、Feishin、Symfonium、箭头音乐等客户端或官方飞牛音乐 App 连接，即可在线搜索、播放网易云 / QQ 音乐 / 酷我 / 酷狗 / 咪咕的歌曲。

> 本项目不内置音源脚本，也不提供音乐资源。请自行准备符合洛雪规范的脚本，并遵守各平台的使用条款。

## 特性

- **聚合搜索与播放**：五平台搜索、原生榜单、同步歌词（含翻译）；默认 302 直链，可切换强制 302 或服务端代理
- **可靠选源**：多音源并发取链，服务端实际解码音频片段验证可播放性，失败自动降档、换源、跨平台兜底
- **网页控制台**：搜歌、播放队列、歌单收藏与导入（网易云 / QQ / 洛雪备份）、听歌统计、最近播放日志
- **多客户端**：Subsonic / OpenSubsonic（含 `apiKey`），以及实验性的飞牛音乐协议
- **运维**：多用户、完整备份（本地 / WebDAV / 定时）、音源每日自动更新、健康检查、管理员 API 调试凭据
- **安全**：Go 单二进制无 CGO；音源脚本运行在沙箱化的 [goja](https://github.com/dop251/goja) 引擎中，无法访问文件系统

## 快速开始

```bash
mkdir -p lxsc/data/sources && cd lxsc
curl -fsSLO https://raw.githubusercontent.com/lengyuesky/lxsc/main/docker-compose.yml
export LXSC_ADMIN_PASSWORD='至少12个字符的强密码'
docker compose up -d
```

打开 `http://<host>:27880/`，用 `admin` 和上面的密码登录，在「音源」页上传或导入脚本（也可提前放进 `data/sources/`）。

- 镜像 `ghcr.io/lengyuesky/lxsc` 支持 `amd64` / `arm64`；用 `LXSC_IMAGE=ghcr.io/lengyuesky/lxsc:v1.2.3` 固定版本。
- `LXSC_ADMIN_PASSWORD` 只在首次创建账号时生效，建议写入同目录的 `.env`；`data/` 需可由 UID 1000 写入。

<details>
<summary>从源码运行</summary>

需要 Go 1.27+ 和 PATH 中的 FFmpeg（用于音频解码验证）：

```bash
go build -o lxsc ./cmd/lxsc
LXSC_ADMIN_PASSWORD='至少12个字符的强密码' LXSC_DATA_DIR=./data ./lxsc
```

或构建本地镜像：`docker build -t lxsc:local .` 后以 `LXSC_IMAGE=lxsc:local docker compose up -d --pull never` 启动。

</details>

## 连接客户端

登录控制台后打开「客户端使用说明」，页面会给出当前地址、用户名和各客户端的分步说明。

| 客户端 | 服务器类型 | 地址 | 账号 |
|---|---|---|---|
| 音流、Feishin、Symfonium、箭头音乐等 | Subsonic | `http://<host>:27880` | lxsc 用户名 + 密码 |
| 飞牛音乐（实验性） | 飞牛 App 登录页 | `<host>:27880`，HTTPS 开关与实际协议一致 | lxsc 用户名 + 密码 |

- 搜索可用前缀限定平台：`wy:` `tx:` `kw:` `kg:` `mg:`；`local:` 只搜本服务器已缓存的歌曲。
- 飞牛音乐选择「标准音质」时也交付原始音频，不转码；移动网络可保留默认标准设置。缺少时长元数据的歌曲仍需选择「原始音质」。目前仅在 Mac 版 1.0.3 实测，详见 [兼容说明](docs/fnmusic-compatibility.md)。

## 配置

启动项通过环境变量或 `data/config.yaml`（见 [config.example.yaml](config.example.yaml)）设置；搜索、播放、缓存等运行时选项在控制台「设置」中修改，即时生效。

| 环境变量 | 说明 | 默认 |
|---|---|---|
| `LXSC_LISTEN` / `LXSC_PORT` | 监听地址 | `:8080` |
| `LXSC_DATA_DIR` | 数据目录 | `./data`（Docker 为 `/data`） |
| `LXSC_ADMIN_USER` / `LXSC_ADMIN_PASSWORD` | 首次启动创建的管理员 | `admin` / 无，必须设置 |
| `LXSC_PROXY` | 上游代理（`http://` 或 `socks5://`） | 空 |
| `LXSC_SDK_WORKERS` | 搜索 / 歌词 JS 线程数 | `2` |
| `LXSC_LOG_LEVEL` | `debug` / `info` / `warn` / `error` | `info` |
| `LXSC_SECRET_KEY` | 口令加密密钥，留空自动生成 | 空 |
| `LXSC_TRUST_PROXY` / `LXSC_TRUSTED_PROXIES` | 信任反向代理的转发头及其 CIDR | `false` / 空 |

**部署提示**：公网访问请使用 HTTPS；反向代理需正确设置 `X-Forwarded-Proto` / `X-Forwarded-For`。升级前先在「备份」页导出完整备份，数据库迁移后旧版本无法直接打开。`GET /healthz` 可用于健康检查。

## 开发

```bash
bash scripts/check.sh                      # Go 检查与测试
npm --prefix tests/web ci --ignore-scripts # 浏览器验收依赖
node tests/web/browser.cjs                 # 浏览器验收（隔离环境、合成音频）
```

修改 `js-bridge/` 后需用 Bun 1.3.14 重新打包：`cd js-bridge && bun install --frozen-lockfile && bun run build`。

## 文档

- [音源选择设计（ADR 0005）](docs/adr/0005-playback-selection-single-race.md) · [音频内容验证](docs/audio-validation.md) · [音源自动更新](docs/source-updates.md)
- [飞牛音乐兼容](docs/fnmusic-compatibility.md) · [标准设置使用原始音频验收](docs/fnmusic-standard-original-verification-20261114.md) · [管理员 API 调试](docs/debug-api.md)
- 各项验收记录见 [docs/](docs/)

## 许可证

原创部分采用 [Apache License 2.0](LICENSE)；第三方代码适用各自许可，详见 [NOTICE](NOTICE)、[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) 和[许可归档](licenses/README.md)。

lxserver 与 lx-music-desktop 的 README 含有关于非商业性质、版权数据和使用限制的补充协议，原文已保留。Apache 标签不代表可无限制商用；如有疑问，请向权利人或专业人士确认。
