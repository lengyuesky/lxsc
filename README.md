# lxsc

[源码仓库](https://github.com/lengyuesky/lxsc) · [Actions 构建记录](https://github.com/lengyuesky/lxsc/actions/workflows/images.yml)

支持 **洛雪音乐自定义音源脚本** 的 Subsonic / OpenSubsonic 服务端。用音流、Feishin、Symfonium 等任意 Subsonic 客户端连接，即可在线搜索、播放网易云 / QQ 音乐 / 酷我 / 酷狗 / 咪咕的歌曲，并有收藏、歌单、榜单、同步歌词（含翻译）。

- Go 编写，单二进制、无 CGO，Docker 镜像约 30 MB，常驻内存约 30–60 MB
- 音源脚本与各平台搜索 SDK 运行在内置的 [goja](https://github.com/dop251/goja) JS 引擎中，只暴露 `lx` 对象等白名单全局，脚本无法访问文件系统
- 多用户账号、Web 控制台（搜歌、连续播放、歌单收藏，以及音源、用户、设置和日志管理）
- 播放默认 `302` 重定向到音源直链，也可切换为服务端代理转发

> 本项目不内置任何音源脚本，也不提供音乐资源。请自行准备符合洛雪音源规范的脚本，并遵守相关平台的使用条款。

## 快速开始（Docker / GHCR）

镜像配置地址为 `ghcr.io/lengyuesky/lxsc`，支持 `linux/amd64` 和 `linux/arm64`。**首次 Actions 发布成功且 GHCR 包单独设置为 Public 后**，才能匿名拉取；仓库公开不等于镜像包自动公开。若镜像尚未就绪，可使用下方的本地源码构建。

```bash
mkdir -p lxsc/data/sources && cd lxsc
curl -fsSLO https://raw.githubusercontent.com/lengyuesky/lxsc/main/docker-compose.ghcr.yml
export LXSC_ADMIN_PASSWORD='请替换为自己的强密码'
# 把音源脚本放到 data/sources/ 目录会在启动时自动导入，也可之后在管理页上传
docker compose -f docker-compose.ghcr.yml pull
docker compose -f docker-compose.ghcr.yml up -d
```

`docker-compose.ghcr.yml` 是独立示例，不要与现有 `docker-compose.yml` 叠加使用。可通过 `LXSC_IMAGE=ghcr.io/lengyuesky/lxsc:v1.2.3` 选择已发布版本，或用 `sha-<完整提交 SHA>` 固定构建；示例标签不代表该版本已经发布。请确保绑定的 `data/` 可由容器 UID 1000 写入。

访问 `http://<host>:27880/` 进入统一控制台。GHCR 示例要求设置首次管理员口令，用户名默认为 `admin`；账号由 `LXSC_ADMIN_USER` / `LXSC_ADMIN_PASSWORD` 控制，仅首次启动生效。原本地构建 compose 的默认账号仍为 `admin` / `admin`，请在使用前修改口令。

所有账号共用同一登录入口：普通用户可使用「歌单」「搜歌」，管理自己的歌单，并浏览、播放其他用户的公开歌单；管理员还会显示概览、音源、用户、备份、设置和日志，并可管理全部用户歌单及替用户创建歌单。

### Actions 镜像标签与权限

- 推送 `main`：先运行 Go 测试/vet、Node 单元测试、JS 锁定构建与产物一致性检查、许可检查；通过后构建双架构镜像，发布 `latest` 和 `sha-<完整提交 SHA>`。
- 推送 `v*` 标签：执行同样检查，发布与 Git 标签一致的镜像标签（例如 `v1.2.3`）及 SHA 标签，不覆盖 `latest`。
- PR：执行检查和双架构构建，不登录 GHCR、不推送，任务没有包写入权限。
- 手动触发：在 [Actions](https://github.com/lengyuesky/lxsc/actions/workflows/images.yml) 选择运行；仅 `main` 或 `v*` 标签会发布，其他分支只检查/构建。

发布任务仅使用本次运行的 `GITHUB_TOKEN` 和 `packages: write`，没有长期 PAT，也不使用 `pull_request_target`。第三方 Actions 固定完整提交 SHA，核对记录在 `.github/actions-pins.json`。工作流只构建/发布镜像，不更改仓库可见性，不自动部署或重启服务器。

**首次发布须由包管理员另外处理可见性**：在 GitHub 的 `Packages → lxsc → Package settings → Change visibility` 将包设为 `Public`，然后实际验证匿名拉取。GHCR 包默认可能为 Private，仓库权限继承不包含包可见性；若发布受限，还需核对包的 `Manage Actions access` 与组织策略。当前文档说明已实施的配置，远程运行/发布结果以 Actions 和 Packages 页面为准，不预先保证某次运行成功。

### 本地 Docker 构建

现有 `docker-compose.yml` 保持本地 `image: lxsc:latest` 加 `build: .` 的行为不变：

```bash
git clone https://github.com/lengyuesky/lxsc.git
cd lxsc
# 先在 docker-compose.yml 修改首次管理员口令，再构建启动。
docker compose up -d --build
```

已有部署不会因增加 GHCR 示例或 Actions 而自动切换镜像。

### 本地运行

需要 Go 1.27+（以 `go.mod` 为准）；JS 桥接产物已随仓库提交，`go build` 不依赖 Node。

```bash
go build -o lxsc ./cmd/lxsc
LXSC_DATA_DIR=./data ./lxsc
```

修改 `js-bridge/` 下的 JS 后需重新打包（固定使用 [Bun](https://bun.sh) **1.3.14**）：

```bash
cd js-bridge && bun install --frozen-lockfile && bun run build
```

同时提交更新后的 `internal/assets/js/` 与 `js-bridge/bundle-inputs.json`；升级依赖后还须重新审核并更新 [许可归档](licenses/README.md)。禁止用非锁定安装绕过构建错误。

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
- **网页搜歌与播放**：控制台的「搜歌」支持全部平台或单平台搜索，每个平台返回前 20 条。搜索结果、歌单歌曲及歌单内搜索均可播放；底部播放器支持播放/暂停、上一首/下一首、进度、音量和静音。点击歌曲时按当前列表建立队列快照，切换页面或继续搜索不会打断播放；列表播完停止，刷新或退出登录会清空队列。
- **收藏到歌单**：「搜歌」结果和播放器可选择有编辑权限的歌单，也可新建歌单并收藏，确认后立即保存。新歌置顶，同一 ID 不重复加入，跨平台同名歌曲独立保留；不会同时加入 Subsonic 的加星收藏。目标歌单存在本页未保存草稿时，需先处理草稿或选择其他歌单。
- **歌单管理**：Subsonic 客户端可直接创建和维护歌单，也可在统一控制台中编辑信息、移除歌曲和调整顺序。歌单内的「加入草稿」仍需点击「保存歌曲变更」，与即时收藏区分；其他页面已修改歌曲列表时保存会提示冲突，保留本地草稿，不自动覆盖服务器。
- **导入洛雪歌单**：在统一控制台的「歌单」页面上传洛雪音乐导出的歌单备份（`.lxmc` 或 `.json`，支持 `playList_v2` / 旧版 `playList` / `allData` 格式），可预览各列表后选择「每个列表新建歌单」或「追加到当前歌单」；本地歌曲会被跳过，单个歌单最多 2000 首。
- **资料库**：收藏、歌单中的歌曲以及最近播放构成你的“资料库”，歌手/专辑/随机/最近播放等视图基于它生成。在线搜索、榜单及普通歌手/专辑浏览结果只进入短期内存缓存，加入歌单、收藏或实际播放后才持久化。原生歌手页保持轻量；完整内容可从「在线音乐 → 歌手」按平台和每页 50 张专辑逐层打开。
- **歌词**：支持 OpenSubsonic `getLyricsBySongId` 同步歌词；有翻译时作为第二个歌词轨（`lang=zho`）返回；`getLyrics` 返回原文+翻译合并的 LRC。
- **音质**：每个用户可在统一控制台的「歌单」或「搜歌」页面设置默认播放音质，两处同步；网页当前歌曲不被打断，新音质在下一次播放或重试时生效。管理员也可在用户管理中修改；Subsonic 客户端传 `maxBitRate` 会向下限制，歌曲或音源不支持所选音质时自动向下降级。网页不进行音频转码，浏览器不支持某格式时可改为 128k / 320k。

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
- **网页播放器同样遵循播放方式设置**，直连失败不会自动改走代理。HTTPS 页面访问 HTTP 音源、防盗链或浏览器格式限制可能导致直连失败，此时可重试、降低音质，或由管理员检查音源与播放方式。网页音频通过现有 HttpOnly 登录 Cookie 认证，不需要把 Subsonic 密码/API Key 放进地址，也不会扩大 `/rest` 的认证范围。

### 网页接口兼容性

`/api/app` 使用网页登录会话，普通用户与管理员遵循原有歌单权限：

- `POST /search`：保持 `{query,sources}` 请求及歌曲数组响应，歌曲对象新增 `duration`（秒，未知为 0）。
- `GET /stream?id=…&quality=…`：网页播放；音质可省略，省略时使用用户默认值，不能用 `proxy` 参数覆盖服务器设置。
- `POST /playlists/{id}/tracks`：用 `{trackId}` 增量置顶收藏，返回 `{playlist,added}`；重复歌曲的 `added` 为 `false`。
- `POST /playlists`：新增可选 `trackIds`，与歌单及元数据在同一事务创建；不传时仍创建空歌单。
- 歌单详情新增 `tracksRevision`；`PUT /playlists/{id}/tracks` 可携带 `expectedRevision`，不匹配返回 HTTP 409。本次保存返回自身事务的列表快照与版本，避免并发收藏的版本被错误用于续写草稿；旧调用不传版本时保留原有完整替换契约，包括顺序及重复项。

### 开发验证

本次搜歌、网页播放与收藏的交付范围、测试结果及上线边界见 [验收记录](docs/search-playback-verification.md)。

```bash
go test ./...
go test -race ./...
go vet ./...
node --test tests/*.test.mjs tests/web/*.test.cjs
node scripts/licenses.mjs --check
```

许可检查前需用固定 Bun 安装依赖并重建 JS；Node 验证使用 26.8.1，标准库许可归档对应 Go 1.27.0。

自动回归使用临时数据库、可控上游和测试音源，不需要真实音乐资源。Node 仅用于前端测试，不是服务运行或 Go 构建依赖。

本机准备好 Playwright 和 Chromium 后，还可执行真实浏览器验收：

```bash
node tests/web/browser.cjs
# 若 Playwright 不在默认模块路径，指定其绝对包路径；也可复用已安装的 Chromium：
LXSC_PLAYWRIGHT_MODULE=/path/to/node_modules/playwright \
LXSC_CHROMIUM_PATH=/path/to/chrome \
node tests/web/browser.cjs
```

浏览器验收会临时构建 `tests/web/fixture`，启动仅监听本机的隔离服务，以合成 WAV 验证播放、拖动、直连/代理、收藏和异步竞态；结束时清理测试服务及数据库，并打印截图和 JSON 结果目录。不会使用 `data/` 或连接外部音乐平台。静态页面随二进制嵌入，修改网页后需重新构建并单独部署才会在运行服务中生效。

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
licenses            第三方许可原文、来源摘要与相关源码副本
scripts/licenses.mjs 许可覆盖与原文校验
.github/workflows   检查与双架构 GHCR 镜像配置
```

`js-bridge/vendor` 来自 [XCQ0607/lxserver](https://github.com/XCQ0607/lxserver)（其上游为 [lx-music-desktop](https://github.com/lyswhut/lx-music-desktop)），固定核对版本、原始导入版本的不确定性与修改记录见 [PATCHES.md](js-bridge/vendor/PATCHES.md)。

## 许可证

本项目有权授权的原创部分采用 [Apache License 2.0](LICENSE)。第三方代码继续适用各自许可，不会被根 LICENSE 重新授权。完整范围、来源、必要声明与 MPL/LGPL 对应源码获取方式见 [NOTICE](NOTICE)、[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) 和 [许可归档](licenses/README.md)；镜像内副本位于 `/usr/share/licenses/lxsc/`。

lxserver 和 lx-music-desktop 的 README 还明确包含补充协议及其优先表述，涉及非商业性质、版权数据和使用限制，原文已保留。不能仅凭 Apache 标签保证无限制商用，也不能以“学习用途”或“24 小时内删除”代替实际许可与法律义务；存在适用疑问时请向相关权利人或专业人士确认。
