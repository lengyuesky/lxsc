# lxsc

支持 **洛雪音乐自定义音源脚本** 的 Subsonic / OpenSubsonic 服务端。用音流、Feishin、Symfonium 等任意 Subsonic 客户端连接，即可在线搜索、播放网易云 / QQ 音乐 / 酷我 / 酷狗 / 咪咕的歌曲，并有收藏、歌单、榜单、同步歌词（含翻译）。

- Go 编写，单二进制、无 CGO，Docker 镜像约 30 MB，常驻内存约 30–60 MB
- 音源脚本与各平台搜索 SDK 运行在内置的 [goja](https://github.com/dop251/goja) JS 引擎中，只暴露 `lx` 对象等白名单全局，脚本无法访问文件系统
- 多用户账号、Web 管理页（上传/导入音源、用户管理、运行设置、日志）
- 播放默认 `302` 重定向到音源直链，也可切换为服务端代理转发

> 本项目不内置任何音源脚本，也不提供音乐资源。请自行准备符合洛雪音源规范的脚本，并遵守相关平台的使用条款。

## 快速开始（Docker）

```bash
mkdir -p lxsc/data/sources && cd lxsc
curl -O https://raw.githubusercontent.com/<you>/lxsc/main/docker-compose.yml
# 把音源脚本放到 data/sources/ 目录会在启动时自动导入，也可之后在管理页上传
docker compose up -d
```

访问 `http://<host>:27880/` 进入统一控制台，默认账号 `admin` / `admin`（由环境变量 `LXSC_ADMIN_USER` / `LXSC_ADMIN_PASSWORD` 控制，仅首次启动生效）。

所有账号共用同一登录入口：普通用户只显示歌单功能，可管理自己的歌单并只读浏览公开歌单；管理员还会显示概览、音源、用户、备份、设置、搜索测试和日志，并可管理全部用户歌单及替用户创建歌单。

### 本地运行

需要 Go 1.27+（以 `go.mod` 为准）；JS 桥接产物已随仓库提交，`go build` 不依赖 Node。

```bash
go build -o lxsc ./cmd/lxsc
LXSC_DATA_DIR=./data ./lxsc
```

修改 `js-bridge/` 下的 JS 后需重新打包（需要 [Bun](https://bun.sh)）：

```bash
cd js-bridge && bun install && bun run build
```

## 客户端连接

在音流等客户端中选择 Subsonic / Navidrome 类型的服务器：

| 项目 | 值 |
|---|---|
| 地址 | `http://<host>:27880` |
| 用户名 / 密码 | 管理页中创建的任意用户 |

支持 `u+p`、`u+t+s`（token）与 OpenSubsonic `apiKey` 三种认证方式。

### 使用方式

- **搜索**：直接输入歌名/歌手会聚合所有平台的结果；用前缀限定平台：`wy:晴天`、`tx:`、`kw:`、`kg:`、`mg:`；`local:` 只搜本服务器已缓存的歌曲。
- **榜单**：榜单会同时出现在原生歌单列表和「在线音乐」目录中；列表阶段只读取各平台返回的全部榜单名称，打开具体榜单后才加载歌曲。榜单歌曲浏览只进入短期内存缓存，不写入资料库；若客户端后台主动请求 `getPlaylist`，服务端无法区分它与用户点击，但不会因此持久化歌曲元数据。
- **歌单管理**：Subsonic 客户端可直接创建和维护歌单，也可在根路径 `/` 的统一控制台中搜索加歌、编辑信息、移除歌曲和调整顺序；新添加的歌曲默认放在歌单最前方。
- **导入洛雪歌单**：在统一控制台的「歌单」页面上传洛雪音乐导出的歌单备份（`.lxmc` 或 `.json`，支持 `playList_v2` / 旧版 `playList` / `allData` 格式），可预览各列表后选择「每个列表新建歌单」或「追加到当前歌单」；本地歌曲会被跳过，单个歌单最多 2000 首。
- **资料库**：收藏、歌单中的歌曲以及最近播放构成你的“资料库”，歌手/专辑/随机/最近播放等视图基于它生成。在线搜索、榜单及普通歌手/专辑浏览结果只进入短期内存缓存，加入歌单、收藏或实际播放后才持久化。原生歌手页保持轻量；完整内容可从「在线音乐 → 歌手」按平台和每页 50 张专辑逐层打开。
- **歌词**：支持 OpenSubsonic `getLyricsBySongId` 同步歌词；有翻译时作为第二个歌词轨（`lang=zho`）返回；`getLyrics` 返回原文+翻译合并的 LRC。
- **音质**：每个用户可在统一控制台的「歌单」页面设置默认播放音质，管理员也可在用户管理中修改；客户端传 `maxBitRate` 会向下限制，歌曲或音源不支持所选音质时自动向下降级。

## 配置

启动项通过环境变量（优先）或 `data/config.yaml`（见 `config.example.yaml`）：

| 环境变量 | 说明 | 默认 |
|---|---|---|
| `LXSC_LISTEN` / `LXSC_PORT` | 监听地址 | `:8080` |
| `LXSC_DATA_DIR` | 数据目录（数据库、密钥、`sources/`） | `./data`（Docker 为 `/data`） |
| `LXSC_ADMIN_USER` / `LXSC_ADMIN_PASSWORD` | 首次启动创建的管理员 | `admin` / `admin` |
| `LXSC_PROXY` | 上游代理 `http://` 或 `socks5://` | 空 |
| `LXSC_SDK_WORKERS` | 搜索/歌词 SDK 的 JS 线程数 | `2` |
| `LXSC_LOG_LEVEL` | `debug` / `info` / `warn` / `error` | `info` |
| `LXSC_SECRET_KEY` | 用户口令加密密钥（留空自动生成到 `data/secret.key`） | 空 |

运行时设置（搜索平台顺序、播放/封面模式、缓存时间、榜单展示等）在管理页「设置」中修改，立即生效。

### 缓存与播放恢复

- 直链与搜索缓存的 TTL 按非负整数秒配置：`0` 表示关闭结果缓存，正数按实际秒数过期。默认分别为 900、600 秒；即使关闭缓存，同时进行的相同请求仍会合并。
- 修改 TTL 只清空对应缓存；修改服务器名称等无关设置不清缓存。音源新增、启停、重载、删除、更新脚本或调整优先级后，旧直链缓存自动失效，已开始传输的音频不受影响。
- 聚合搜索保持平台交错顺序；部分平台失败时返回成功平台的结果，但不缓存残缺结果。单个客户端取消不影响其他仍在等待的相同请求；无人等待时取消共享请求。
- **代理播放与下载**遇到上游 `403/404/410` 时，在向客户端发送内容前最多重新取链并重试一次，保留 Range/If-Range 和原有音质降级规则。网络错误、`429/5xx`、`416` 或传输中断不重试；`416` 及 Content-Range 按媒体协议透传。
- 两次取链共用累计 45 秒预算，不限制整首音频传输时长。默认的 **302 模式**不探测直链，也不自动重试或切换代理；服务端无法确认客户端直连是否成功。

### 开发验证

```bash
go test ./...
go test -race ./...
go vet ./...
```

自动回归使用临时数据库、可控上游和测试音源，不需要真实音乐资源。

## 完整备份与 WebDAV

管理员可在统一控制台的「备份」页面管理完整迁移备份：

- **本地导出**：生成 `.lxsc-backup` 文件，包含 SQLite 一致性快照、用户/歌单/设置/音源、有效迁移密钥、`config.yaml` 与 `data/sources/`。可选填写独立备份密码进行 Age scrypt 加密。
- **本地导入**：上传备份后先检查格式、清单校验和、路径和 SQLite 完整性，再暂存恢复内容；当前服务不会立即被覆盖。
- **恢复生效**：导入成功后执行 `docker compose restart`。启动时会先保存最近一个本地回滚副本，再应用恢复；目标实例的监听地址、数据目录和加密密钥保持不变，用户密码会安全地重新加密到目标密钥。
- **WebDAV**：支持 Basic Auth/应用密码、连接测试、立即上传、远程列表、下载、恢复和删除。WebDAV 密码及远程备份加密密码均加密存储。
- **自动备份**：支持每日或每周按服务器时区执行；成功上传后默认只保留最近 3 份，设置为 `0` 可关闭自动清理。上传失败不会删除旧备份。

完整备份包含可用于迁移用户口令的敏感密钥。未设置备份密码时，请只存放在受信任位置；远程 WebDAV 推荐使用 HTTPS。

## 已实现的 Subsonic 端点

`ping getLicense getOpenSubsonicExtensions getUser getUsers getScanStatus getMusicFolders getIndexes getArtists getMusicDirectory getGenres getArtist getArtistInfo(2) getAlbum getAlbumInfo(2) getSong getTopSongs getSimilarSongs(2) getAlbumList(2) getRandomSongs getSongsByGenre getNowPlaying getStarred(2) search search2 search3 getPlaylists getPlaylist createPlaylist updatePlaylist deletePlaylist stream download getCoverArt getLyrics getLyricsBySongId star unstar scrobble setRating savePlayQueue getPlayQueue getBookmarks getInternetRadioStations getPodcasts getShares getVideos`

响应同时支持 JSON 与 XML，支持 `formPost` 扩展。

## 音源脚本兼容性

服务端实现了与洛雪桌面版 `preload.js` 相同的 `lx` 对象：`lx.request`、`lx.send/on`、`lx.utils.crypto/buffer/zlib`、`lx.currentScriptInfo`，以及常见脚本依赖的全局（`Buffer`、`atob/btoa`、`TextEncoder`、`URL`、`crypto`、`navigator` 等）。脚本按 `@name/@version` 等头注释识别，`inited` 上报的平台与音质决定其能力；多个脚本按优先级依次尝试。

goja 不支持极少数 V8 特有行为，如果某个脚本加载失败，可在管理页查看该音源的日志。

## 目录结构

```
cmd/lxsc            入口
internal/js         goja 运行时、宿主函数、音源管理、SDK 工作池
internal/music      元数据模型、ID 编码、聚合搜索、歌词、缓存
internal/subsonic   Subsonic API
internal/admin      管理接口
internal/backup     完整备份、恢复暂存、WebDAV 与定时任务
internal/assets     内嵌的 JS 产物与统一控制台
js-bridge           JS 桥接工程（prelude、shims、vendor 的 musicSdk）
```

`js-bridge/vendor` 来自 [XCQ0607/lxserver](https://github.com/XCQ0607/lxserver)（其上游为 [lx-music-desktop](https://github.com/lyswhut/lx-music-desktop)），修改记录见 `js-bridge/vendor/PATCHES.md`。

## 许可证

Apache-2.0
