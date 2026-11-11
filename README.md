# lxsc

[源码仓库](https://github.com/lengyuesky/lxsc) · [Actions 构建记录](https://github.com/lengyuesky/lxsc/actions/workflows/images.yml)

支持 **洛雪音乐自定义音源脚本** 的 Subsonic / OpenSubsonic 服务端。用音流、Feishin、Symfonium 等任意 Subsonic 客户端连接，即可在线搜索、播放网易云 / QQ 音乐 / 酷我 / 酷狗 / 咪咕的歌曲，并有收藏、歌单、榜单、同步歌词（含翻译）。

> 本项目不内置任何音源脚本，也不提供音乐资源。请自行准备符合洛雪音源规范的脚本，并遵守相关平台的使用条款。

## 特性

- Go 编写，单二进制、无 CGO，Docker 镜像约 30 MB，常驻内存约 30–60 MB
- 音源脚本与各平台搜索 SDK 运行在内置的 [goja](https://github.com/dop251/goja) JS 引擎中，只暴露 `lx` 对象等白名单全局，脚本无法访问文件系统
- 多用户账号、Web 统一控制台（搜歌、连续播放、歌单收藏，以及音源、用户、设置、备份和日志管理）
- 播放默认 `302` 重定向到音源直链，可切换为禁止音频转发的强制 `302` 模式或服务端代理转发
- 五平台聚合搜索、原生榜单、歌单导入（网易云 / QQ 链接、洛雪备份）与订阅更新、听歌统计、网页播放器带同步歌词
- 兼容音流、Feishin、Symfonium、箭头音乐等客户端；支持 OpenSubsonic `apiKey` 与设备密钥管理
- 管理员可创建限时 API 调试凭据（含机器可读指南），私下交给受信任的人或 AI 排查客户端问题，详见 [docs/debug-api.md](docs/debug-api.md)
- 完整备份（本地 / WebDAV / 自动定时）与恢复、健康检查端点

## 快速开始（Docker / GHCR）

`docker-compose.yml` 使用公开镜像 `ghcr.io/lengyuesky/lxsc:latest`（`linux/amd64` + `linux/arm64`，可匿名拉取，无需本地构建或登录 GHCR）：

```bash
mkdir -p lxsc/data/sources && cd lxsc
curl -fsSLO https://raw.githubusercontent.com/lengyuesky/lxsc/main/docker-compose.yml
export LXSC_ADMIN_PASSWORD='请替换为至少12个字符的强密码'
# 把音源脚本放到 data/sources/ 会在启动时自动导入，也可之后在管理页上传
docker compose pull
docker compose up -d
```

- 用 `LXSC_IMAGE=ghcr.io/lengyuesky/lxsc:v1.2.3` 选择已发布版本，或用 `sha-<完整提交 SHA>` 固定构建；绑定的 `data/` 需可由容器 UID 1000 写入。
- 首次创建账号必须显式设置至少 12 个字符、且不同于用户名的 `LXSC_ADMIN_PASSWORD`（程序无默认密码，用户名默认 `admin`）。该变量仅首次启动创建时生效，不会覆盖已有账号；后续 Compose 命令也需提供，建议保存到 Compose 同目录的 `.env`（仓库已忽略），并限制文件权限。
- 访问 `http://<host>:27880/` 进入统一控制台。普通用户可使用「歌单」「搜歌」并播放公开歌单；管理员额外显示概览、音源、用户、备份、设置和日志。

### Actions 镜像标签

- 推送 `main` 通过检查后发布 `latest` 与 `sha-<完整提交 SHA>`；推送 `v*` 标签发布同名镜像标签（如 `v1.2.3`）及 SHA 标签，不覆盖 `latest`；PR 只检查和构建，不推送。
- 发布仅使用本次运行的 `GITHUB_TOKEN` 与 `packages: write`，无长期 PAT；第三方 Actions 固定完整提交 SHA（`.github/actions-pins.json`）。
- Fork 或首次发布到新包后，须在 GitHub `Packages → lxsc → Package settings` 将包可见性设为 `Public` 并实际验证匿名拉取。

### 本地 Docker 构建

默认 `docker-compose.yml` 不含 `build: .`。从源码构建时手动构建镜像，再复用同一份 Compose 配置：

```bash
git clone https://github.com/lengyuesky/lxsc.git
cd lxsc
export LXSC_ADMIN_PASSWORD='请替换为至少12个字符的强密码'
export LXSC_IMAGE=lxsc:local
docker build -t "$LXSC_IMAGE" .
docker compose up -d --pull never
```

更新源码后需重新执行 `docker build`。更新仓库文件不会自动部署或重启已有服务。

### 本地运行

需要 Go 1.27+（以 `go.mod` 为准）；JS 桥接产物已随仓库提交，`go build` 不依赖 Node。

```bash
go build -o lxsc ./cmd/lxsc
# 仅首次创建账号需要初始化密码，已有账号不会被覆盖。
LXSC_ADMIN_PASSWORD='请替换为至少12个字符的强密码' LXSC_DATA_DIR=./data ./lxsc
```

修改 `js-bridge/` 下的 JS 后需用固定版本 [Bun](https://bun.sh) **1.3.14** 重新打包，并提交更新后的 `internal/assets/js/` 与 `js-bridge/bundle-inputs.json`；升级依赖后须重新审核[许可归档](licenses/README.md)：

```bash
cd js-bridge && bun install --frozen-lockfile && bun run build
```

## 客户端连接

登录控制台后，打开侧栏「客户端使用说明」（`/#clients`），普通用户和管理员均可查看。页面提供当前服务器地址、本人用户名的一键复制，箭头音乐、音流、Feishin、Symfonium 的手动连接步骤，以及搜索、歌单、音质、歌词和常见问题说明；不会读取或展示密码。API Key 说明仅管理员可见，密钥管理和 API 调试权限不向普通用户开放。

| 项目 | 值 |
|---|---|
| 服务器类型 | Subsonic / Navidrome |
| 地址 | `http://<host>:27880` |
| 用户名 / 密码 | 管理页中创建的任意用户 |

支持 `u+p`、`u+t+s`（token）与 OpenSubsonic `apiKey` 三种认证方式。管理员可在「用户 → 管理 API Key」为设备生成带名称与有效期的密钥并逐个撤销。

### 飞牛音乐（实验性）

新增独立的飞牛音乐协议入口，原版 App 使用服务器根地址及现有 lxsc 用户名、密码连接，不需要部署 fnOS 或借官方 NAS 完成认证。它不是 Subsonic 连接方式，也不提供 FN ID、FN Connect 或 NAS 系统账号登录。

- 已在 Mac 上运行的飞牛音乐 1.0.3（构建 103006）实测初始化、音乐独立账号登录、歌单与曲目展示、合成 WAV 播放，以及用户指定独家音源两首真实 FLAC 的播放、暂停/恢复、拖动、手动切歌和自然结束后自动切歌。范围仅限该平台、版本、音源与原始音质组合，不代表 iPhone、Android、其他音源或所有未来版本已兼容。
- App 的 Wi-Fi 和移动网络播放均需选择「原始音质」；Mac 可能按移动网络偏好选择音质。标准音质会请求转码/HLS，本服务明确不支持，不以虚假的就绪响应兜底。
- 「原始音质」表示不在 lxsc 中转码；音源取链仍沿用该 lxsc 用户的默认音质和既有降档策略，不保证最高档、原录音或所有上游均可直连。
- 媒体交付复用现有播放设置。默认直链/302，不为飞牛自动开启代理或音频落盘；管理员已选择的强制 302 仍禁止音频转发。公网部署必须使用 HTTPS，密码摘要也是可重放凭据。
- 曲目、歌单、专辑和歌手封面复用安全取图、配置回退、缓存及并发限制，不再固定返回占位图；封面 ID 带资源版本，升级时不用清空 App 缓存。歌词复用平台 SDK → 音源 → 管理员配置的备用接口，支持 LRC、毫秒偏移及翻译；原版 Mac App 已实测两首原曲的真实封面、歌词显示和同步高亮。
- 歌曲、专辑、歌手搜索复用现有五平台音乐核心；歌单搜索只返回本用户可读歌单。搜索结果不自动收藏、加入歌单或批量落库，临时访问权限限定到当前音乐会话并在 30 分钟后失效；主动播放仍沿用现有元数据记录策略。返回的 `total` 是本次有界搜索结果数量，不冒充全平台总数。原版 Mac App 已实际搜索并播放库外酷狗《庐州月》，暂停/恢复与同步歌词通过；酷我同名结果本轮取链超时，不能推定所有搜索结果均可播放。
- 仍不提供飞牛 App 内的歌单/收藏写入、离线下载、标准转码/HLS 和本地文件扫描；未实现的写接口返回明确错误，现有 Web/Subsonic 功能不受此限制。
- 数据库新增可再生的客户端标识映射表；升级前先备份，已迁移的数据不能由不支持该数据库版本的旧二进制直接打开。

完整证据、尚未通过的验收项及安装包校验限制见 [docs/fnmusic-compatibility.md](docs/fnmusic-compatibility.md)。当前属于初版适配，不是完整兼容承诺。

## 使用

- **账号与密码**：所有登录用户都可通过侧栏底部「修改密码」验证当前密码后修改本人密码，新密码为 8–128 个字符且不能全为空白；成功后本人所有网页登录会话失效，管理员已有的临时调试凭据也会撤销。使用密码连接的 Subsonic 客户端需更新密码，设备 API Key 不受影响。管理员在「用户」页创建账号时可勾选「随机密码」，由服务端生成 24 位安全随机密码；仅创建成功后展示一次，可复制，关闭或退出登录后清除展示。未勾选时仍手动填写密码。
- **搜索**：直接输入歌名/歌手会聚合所有平台的结果；用前缀限定平台：`wy:晴天`、`tx:`、`kw:`、`kg:`、`mg:`；`local:` 只搜本服务器已缓存的歌曲。
- **榜单**：同时出现在原生歌单列表和「在线音乐」目录中，打开后返回全部可用歌曲；管理员可在「歌单 → 自定义榜单」按平台选择展示全部或勾选榜单。
- **网页搜歌与播放**：控制台「搜歌」支持全部或单平台搜索，支持独立加载更多与失败重试；底部播放器带队列面板、随机/循环模式，可播放搜索结果与歌单歌曲；刷新后按账号恢复本机最近 30 天内的队列与进度。
- **听歌统计**：「听歌统计」提供近 7/30/90/365 天的时长、日均、趋势、日历及歌曲排行；管理员可查看全站、用户排行并筛选指定用户。
- **收藏与歌单**：搜歌结果可收藏到有编辑权限的歌单或新建歌单，新歌置顶、不重复；Subsonic 客户端也可直接创建和维护歌单。歌单内的「加入草稿」需点击「保存歌曲变更」后生效。
- **导入歌单**：「歌单 → 导入歌单」支持网易云 / QQ 公开歌单的链接或数字 ID、洛雪 `.lxmc`/`.json` 备份，预览后再导入；仅保存歌曲元数据，不下载音频，也不接收平台账号或 Cookie。网易云 / QQ 歌单还可在「订阅更新」保存来源、按需或每天自动追加新歌。
- **其他工具**：「换版本」按歌名、歌手、时长查找其他版本，保留最近 10 次保存历史；「个人智能歌单」提供近 30 天常听、本月最爱等智能列表；管理员可在「音源」页查看取链调用统计与测试音源。
- **音源更新**：通过 URL 导入时默认开启每日自动更新，按北京时间凌晨 3:00 检查；停机后补查最近一次过期计划，停用音源时暂停。管理员可在音源卡片的「更新设置」补填或修改来源、开关自动更新，也可点击「立即检查更新」。内容变化且新脚本初始化成功后才替换；失败保留旧脚本，最多再重试两次，间隔为 5 分钟、30 分钟。详情见 [音源自动更新](docs/source-updates.md)。
- **歌词**：支持 OpenSubsonic `getLyricsBySongId` 同步歌词，有翻译时作为第二个歌词轨（`lang=zho`）返回；`getLyrics` 返回原文+翻译合并的 LRC。
- **音质**：每个用户可在控制台设置默认播放音质；Subsonic 客户端传 `maxBitRate` 会向下限制，歌曲或音源不支持所选音质时自动降级；网页不做音频转码。
- **箭头音乐（Amcfy Music）**：选择 **Subsonic** 类型连接。艺术家/专辑搜索与歌曲搜索独立处理，打开榜单即返回完整快照并缓存，空榜单不会被误当成空歌单。

## 配置

启动项通过环境变量（优先）或 `data/config.yaml`（见 `config.example.yaml`）：

| 环境变量 | 说明 | 默认 |
|---|---|---|
| `LXSC_LISTEN` / `LXSC_PORT` | 监听地址 | `:8080` |
| `LXSC_DATA_DIR` | 数据目录（数据库、密钥、`sources/`） | `./data`（Docker 为 `/data`） |
| `LXSC_ADMIN_USER` / `LXSC_ADMIN_PASSWORD` | 首次启动创建的管理员 | 用户名 `admin`；无默认密码，首次创建必须设置至少 12 个字符 |
| `LXSC_PROXY` | 上游代理 `http://` 或 `socks5://` | 空 |
| `LXSC_SDK_WORKERS` | 搜索/歌词 SDK 的 JS 线程数 | `2` |
| `LXSC_LOG_LEVEL` | `debug` / `info` / `warn` / `error` | `info` |
| `LXSC_SECRET_KEY` | 用户口令加密密钥（留空自动生成到 `data/secret.key`） | 空 |
| `LXSC_TRUST_PROXY` | 是否接受可信代理的转发头，启用时必须同时配置可信网段 | `false` |
| `LXSC_TRUSTED_PROXIES` | 可信反代 CIDR，环境变量用逗号分隔；YAML 为 `trusted_proxies` 数组 | 空 |

- 运行时设置（搜索平台顺序、播放/封面模式、缓存时间等）在管理页「设置」中修改，保存后立即生效；榜单展示在「歌单 → 自定义榜单」中配置。
- 反向代理必须覆盖 `X-Forwarded-Proto` 并正确追加 `X-Forwarded-For`；服务端只信任 `LXSC_TRUSTED_PROXIES` 内的代理链。只有真实 TLS 或可信代理标记的 HTTPS 才设置 Secure Cookie。
- **播放方式**（设置 → 播放与缓存）：默认 302 重定向到音源直链（保留客户端 `proxy=1` 的兼容）；「强制 302」禁止服务器转发音频，即使客户端请求代理；「服务端代理转发」由服务器拉取并转发。修改对后续播放生效，不中断已开始的传输。
- **歌词与封面备用接口**（设置 → 歌词与封面备用接口）：默认使用 [LrcAPI 公共服务](https://docs.lrc.cx/docs/QuickStart)，可分别修改地址或留空关闭；封面支持 URL 模板占位符（`{title}`、`{artist}`、`{album}`、`{id}`、`{source}` 等）。外部图源仅连接公网 HTTP(S) 标准端口并逐跳校验重定向与 DNS，防 SSRF。
- **直链缓存**：提供不缓存/1 天/1 周/1 个月/永久五档，默认 1 周，保存在独立的 `data/url-cache.db` 中，重启后继续复用；音源变更后自动失效。

- **音源选择**（设置 → 播放与缓存）：默认音质优先，按档位从高到低，每档同时向所有支持该档的音源取链并校验媒体，最快通过者胜出，该档全部失败才整体降档；启动速度优先让所有音源同时开跑、各自源内降档，最高档成功立即采用，较低档先成功时为更高档额外等待最多 200 毫秒。**并发音源数**默认 3（可设 1–10），超出部分在有源失败后按调度评分补位；越大首播越快，但每次播放实际请求的上游越多、越容易被限流。命中有效的直链缓存时只校验缓存链接，不向其它音源并发取链。播放准备共享 9 秒预算（HEAD 预检 4 秒）；代理响应头成功后，正文传输继续使用独立生命周期。降级直链最多缓存 5 分钟，之后重新尝试较高档，永久缓存也遵守此限制。

## Subsonic 功能支持范围

```
ping getLicense getOpenSubsonicExtensions getUser getUsers getScanStatus getMusicFolders getIndexes getArtists getMusicDirectory getGenres getArtist getArtistInfo(2) getAlbum getAlbumInfo(2) getSong getTopSongs getSimilarSongs(2) getAlbumList(2) getRandomSongs getSongsByGenre getNowPlaying getStarred(2) search search2 search3 getPlaylists getPlaylist createPlaylist updatePlaylist deletePlaylist stream download getCoverArt getLyrics getLyricsBySongId star unstar scrobble setRating savePlayQueue getPlayQueue getBookmarks getInternetRadioStations getPodcasts getShares getVideos
```

响应同时支持 JSON 与 XML，支持 `formPost` 扩展。`savePlayQueue/getPlayQueue` 按用户持久化队列、当前歌曲与毫秒进度；`setRating` 支持歌曲 0–5 星；`scrobble submission=false` 只更新正在播放，不累计收听统计。`getBookmarks/getInternetRadioStations/getPodcasts/getShares/getVideos` 为兼容占位，返回空列表。

## 音源脚本兼容性

服务端实现了与洛雪桌面版 `preload.js` 相同的 `lx` 对象（`lx.request`、`lx.send/on`、`lx.utils.*`、`lx.currentScriptInfo`），以及常见脚本依赖的全局（`Buffer`、`atob/btoa`、`TextEncoder`、`URL`、`crypto`、`navigator` 等）。脚本按 `@name/@version` 等头注释识别，多个脚本按运行表现动态排序取链（成功率与延迟滑动窗口评分，失败源指数退避冷却后自动恢复），冷启动按加载顺序；取链按并发上限同时请求多个音源，先成功者胜出。goja 不支持极少数 V8 特有行为，某个脚本加载失败时可在管理页查看该音源日志。

## 备份与维护

- 管理员可在控制台「备份」页导出完整 `.lxsc-backup` 文件（含 SQLite 一致性快照、用户/歌单/设置/音源、`config.yaml` 与 `data/sources/`），可选独立备份密码进行 Age 加密；导入校验后暂存，执行 `docker compose restart` 生效，启动时自动保留一个回滚副本。
- 支持 WebDAV 远程上传、列表、下载、恢复与删除，以及每日/每周自动备份（默认保留最近 3 份，上传失败不删除旧备份）。
- 备份包含可用于迁移用户口令的敏感密钥；未设置备份密码时请只存放在受信任位置，远程 WebDAV 推荐 HTTPS。直链缓存数据库不进入备份。
- 升级前请保留完整备份：启动时以事务执行版本化迁移，旧版二进制不能打开已迁移数据库，回退应恢复升级前备份。「设置 → 元数据维护」提供播放历史清理与数据库压缩（压缩需在低峰窗口执行）。
- `GET /healthz` 无需登录，检查本地数据库和 SDK 状态；镜像内置每 30 秒一次的健康检查（启动宽限 120 秒）。

## 开发

```bash
bash scripts/check.sh          # Go 检查与测试（--race 附带竞态检测）
GOTOOLCHAIN=go1.27.0 node scripts/licenses.mjs --check   # 许可检查
```

- 许可检查前需用固定 Bun 安装依赖并重建 JS；Node 验证使用 24.21.0 LTS。
- 自动回归使用临时数据库、可控上游和测试音源，不需要真实音乐资源；Node 仅用于前端测试，不是服务运行或 Go 构建依赖。
- 浏览器验收使用独立锁文件固定 Playwright（不在默认模块路径时可用 `LXSC_PLAYWRIGHT_MODULE` / `LXSC_CHROMIUM_PATH` 指定）：

```bash
npm --prefix tests/web ci --ignore-scripts
node tests/web/node_modules/playwright/cli.js install --only-shell chromium
node tests/web/browser.cjs
```

浏览器验收会临时构建 fixture、启动仅监听本机的隔离服务，以合成 WAV 验证播放、收藏等流程，结束时自动清理，不使用 `data/` 或连接外部音乐平台。

## 文档索引

| 文档 | 内容 |
|---|---|
| [docs/fnmusic-compatibility.md](docs/fnmusic-compatibility.md) | 原版飞牛音乐独立服务端：边界、初版实现、Mac 实测与剩余验收 |
| [docs/debug-api.md](docs/debug-api.md) | 管理员 API 调试：凭据、权限与排查流程 |
| [docs/search-playback-verification.md](docs/search-playback-verification.md) | 搜歌、网页播放与歌单收藏验收 |
| [docs/listening-verification.md](docs/listening-verification.md) | 听歌统计验收 |
| [docs/playback-startup-verification.md](docs/playback-startup-verification.md) | 播放启动等待优化验收 |
| [docs/performance-verification.md](docs/performance-verification.md) | 性能与稳定性优化验收 |
| [docs/overload-verification.md](docs/overload-verification.md) | 并发边界与过载保护验收 |
| [docs/hardening-verification.md](docs/hardening-verification.md) | 安全与资源优化验收 |
| [docs/query-optimization-verification.md](docs/query-optimization-verification.md) | 查询与前端优化验收 |
| [docs/hot-path-optimization-verification.md](docs/hot-path-optimization-verification.md) | 热点路径优化验收 |
| [docs/feature-preserving-optimization-verification.md](docs/feature-preserving-optimization-verification.md) | 功能保留优化验收 |
| [docs/improvements-verification.md](docs/improvements-verification.md) | 优化与功能验收 |

## 许可证

本项目有权授权的原创部分采用 [Apache License 2.0](LICENSE)。第三方代码继续适用各自许可，不会被根 LICENSE 重新授权；完整范围、来源、必要声明与对应源码获取方式见 [NOTICE](NOTICE)、[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) 和[许可归档](licenses/README.md)，镜像内副本位于 `/usr/share/licenses/lxsc/`。

lxserver 和 lx-music-desktop 的 README 还明确包含补充协议及其优先表述，涉及非商业性质、版权数据和使用限制，原文已保留。不能仅凭 Apache 标签保证无限制商用，也不能以"学习用途"或"24 小时内删除"代替实际许可与法律义务；存在适用疑问时请向相关权利人或专业人士确认。
