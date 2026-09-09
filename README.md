# lxsc

[源码仓库](https://github.com/lengyuesky/lxsc) · [Actions 构建记录](https://github.com/lengyuesky/lxsc/actions/workflows/images.yml)

支持 **洛雪音乐自定义音源脚本** 的 Subsonic / OpenSubsonic 服务端。用音流、Feishin、Symfonium 等任意 Subsonic 客户端连接，即可在线搜索、播放网易云 / QQ 音乐 / 酷我 / 酷狗 / 咪咕的歌曲，并有收藏、歌单、榜单、同步歌词（含翻译）。

- Go 编写，单二进制、无 CGO，Docker 镜像约 30 MB，常驻内存约 30–60 MB
- 音源脚本与各平台搜索 SDK 运行在内置的 [goja](https://github.com/dop251/goja) JS 引擎中，只暴露 `lx` 对象等白名单全局，脚本无法访问文件系统
- 多用户账号、Web 控制台（搜歌、连续播放、歌单收藏，以及音源、用户、设置和日志管理）
- 播放默认 `302` 重定向到音源直链，也可切换为禁止音频转发的强制 `302` 模式或服务端代理转发
- [限时安全调试](docs/debug-api.md)：管理员创建独立临时 Bearer 凭据，默认只读白名单状态与结构化播放诊断，主动探测需单独授权

> 本项目不内置任何音源脚本，也不提供音乐资源。请自行准备符合洛雪音源规范的脚本，并遵守相关平台的使用条款。

## 快速开始（Docker / GHCR）

默认的 `docker-compose.yml` 直接使用公开镜像 `ghcr.io/lengyuesky/lxsc:latest`，支持 `linux/amd64` 和 `linux/arm64`，可匿名拉取，无需本地构建或登录 GHCR。

```bash
mkdir -p lxsc/data/sources && cd lxsc
curl -fsSLO https://raw.githubusercontent.com/lengyuesky/lxsc/main/docker-compose.yml
export LXSC_ADMIN_PASSWORD='请替换为自己的强密码'
# 把音源脚本放到 data/sources/ 目录会在启动时自动导入，也可之后在管理页上传
docker compose pull
docker compose up -d
```

可通过 `export LXSC_IMAGE=ghcr.io/lengyuesky/lxsc:v1.2.3` 选择已发布版本，或用 `sha-<完整提交 SHA>` 固定构建；示例标签不代表该版本已经发布。请确保绑定的 `data/` 可由容器 UID 1000 写入。

访问 `http://<host>:27880/` 进入统一控制台。Compose 要求显式设置非空的 `LXSC_ADMIN_PASSWORD`，请使用强密码；用户名默认为 `admin`。账号由 `LXSC_ADMIN_USER` / `LXSC_ADMIN_PASSWORD` 控制，仅首次启动创建时生效，不会覆盖已有账号。后续执行 Compose 命令时也需提供该密码变量；可将变量保存到 Compose 文件同目录的 `.env`（仓库已忽略），妥善限制文件权限，不要提交密码。

所有账号共用同一登录入口：普通用户可使用「歌单」「搜歌」，管理自己的歌单，并浏览、播放其他用户的公开歌单；管理员还会显示概览、音源、用户、备份、设置和日志，并可管理全部用户歌单及替用户创建歌单。

### Actions 镜像标签与权限

- 推送 `main`：先运行 Go 测试/vet、Node 单元测试、JS 锁定构建与产物一致性检查、许可检查；通过后构建双架构镜像，发布 `latest` 和 `sha-<完整提交 SHA>`。
- 推送 `v*` 标签：执行同样检查，发布与 Git 标签一致的镜像标签（例如 `v1.2.3`）及 SHA 标签，不覆盖 `latest`。
- PR：执行检查和双架构构建，不登录 GHCR、不推送，任务没有包写入权限。
- 手动触发：在 [Actions](https://github.com/lengyuesky/lxsc/actions/workflows/images.yml) 选择运行；仅 `main` 或 `v*` 标签会发布，其他分支只检查/构建。

发布任务仅使用本次运行的 `GITHUB_TOKEN` 和 `packages: write`，没有长期 PAT，也不使用 `pull_request_target`。第三方 Actions 固定完整提交 SHA，核对记录在 `.github/actions-pins.json`。工作流只构建/发布镜像，不更改仓库可见性，不自动部署或重启服务器。

本仓库的 GHCR 包已公开。**Fork 或首次发布到新包时，须单独确认包可见性**：若为 Private，由包管理员在 GitHub 的 `Packages → lxsc → Package settings → Change visibility` 将包设为 `Public`，然后实际验证匿名拉取。仓库权限继承不包含包可见性；若发布受限，还需核对包的 `Manage Actions access` 与组织策略。各次构建和发布结果以 Actions 和 Packages 页面为准。

### 本地 Docker 构建

默认 `docker-compose.yml` 不再包含 `build: .`。需要从源码构建时，先手动构建本地镜像，再通过 `LXSC_IMAGE` 复用同一份 Compose 配置：

```bash
git clone https://github.com/lengyuesky/lxsc.git
cd lxsc
export LXSC_ADMIN_PASSWORD='请替换为自己的强密码'
export LXSC_IMAGE=lxsc:local
docker build -t "$LXSC_IMAGE" .
docker compose up -d --pull never
```

后续使用本地镜像时，请继续设置 `LXSC_IMAGE=lxsc:local`，或将其保存到 `.env`；更新源码后需重新执行 `docker build`，仅加 `--build` 不会构建镜像。

更新仓库文件不会自动部署或重启已有服务。原本地构建部署若要切换到 GHCR，请先备份 `data/`、设置上述密码变量并取消本地 `LXSC_IMAGE` 覆盖，再执行 `docker compose pull` 和 `docker compose up -d`；已有账号与数据继续保存在原来的 `data/` 中。

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
- **榜单**：榜单会同时出现在原生歌单列表和「在线音乐」目录中；管理员可在「歌单 → 自定义榜单」按平台选择展示全部榜单或仅展示勾选的榜单，两处共用选择。列表阶段只读取榜单名称，打开具体榜单后才加载歌曲。榜单歌曲浏览只进入短期内存缓存，不写入资料库；若客户端后台主动请求 `getPlaylist`，服务端无法区分它与用户点击，但不会因此持久化歌曲元数据。
- **网页搜歌与播放**：控制台的「搜歌」支持全部平台或单平台搜索，每个平台返回前 20 条。搜索结果、歌单歌曲及歌单内搜索均可播放；底部播放器支持播放/暂停、上一首/下一首、进度、音量和静音。点击歌曲时按当前列表建立队列快照，切换页面或继续搜索不会打断播放；列表播完停止，刷新或退出登录会清空队列。
- **听歌统计**：控制台新增「听歌统计」，提供近 7、30、90、365 天的时长、日均、歌曲数、活跃天数、趋势、日历及歌曲排行。普通用户看个人数据，管理员可看全站、用户排行并筛选指定用户，兼容浅色／深色及移动端。
- **收藏到歌单**：「搜歌」结果和播放器可选择有编辑权限的歌单，也可新建歌单并收藏，确认后立即保存。新歌置顶，同一 ID 不重复加入，跨平台同名歌曲独立保留；不会同时加入 Subsonic 的加星收藏。目标歌单存在本页未保存草稿时，需先处理草稿或选择其他歌单。
- **歌单管理**：Subsonic 客户端可直接创建和维护歌单，也可在统一控制台中编辑信息、移除歌曲和调整顺序。歌单内的「加入草稿」仍需点击「保存歌曲变更」，与即时收藏区分；其他页面已修改歌曲列表时保存会提示冲突，保留本地草稿，不自动覆盖服务器。
- **导入洛雪歌单**：在统一控制台的「歌单」页面上传洛雪音乐导出的歌单备份（`.lxmc` 或 `.json`，支持 `playList_v2` / 旧版 `playList` / `allData` 格式），可预览各列表后选择「每个列表新建歌单」或「追加到当前歌单」；本地歌曲会被跳过，单个歌单最多 2000 首。
- **资料库**：收藏、歌单中的歌曲以及最近播放构成你的“资料库”，歌手/专辑/随机/最近播放等视图基于它生成。在线搜索、榜单及普通歌手/专辑浏览结果只进入短期内存缓存，加入歌单、收藏或实际播放后才持久化。原生歌手页保持轻量；完整内容可从「在线音乐 → 歌手」按平台和每页 50 张专辑逐层打开。
- **歌词**：支持 OpenSubsonic `getLyricsBySongId` 同步歌词；有翻译时作为第二个歌词轨（`lang=zho`）返回；`getLyrics` 返回原文+翻译合并的 LRC。
- **音质**：每个用户可在统一控制台的「歌单」或「搜歌」页面设置默认播放音质，两处同步；网页当前歌曲不被打断，新音质在下一次播放或重试时生效。管理员也可在用户管理中修改；Subsonic 客户端传 `maxBitRate` 会向下限制，歌曲或音源不支持所选音质时自动向下降级。网页不进行音频转码，浏览器不支持某格式时可改为 128k / 320k。

### 听歌时长口径

统计从新版首次打开数据库时启用，不回填旧播放历史。日期按北京时间划分，网页跨午夜播放拆分到两天，日均按所选范围内、统计启用后的自然日计算。总时长包含以下两部分，多设备播放分别累计：

- **网页实听**：只累计音频正常推进时实际经过的时间，暂停、缓冲、拖动和失败不计时；后台正常播放继续累计。每 15 秒上报，暂停、切歌、结束和离开页面时补报。重复或乱序累计值不会重复计时，短暂网络失败在当前页面保留快照重试。浏览器被强制关闭或离线退出时，尚未送达的进度可能丢失。
- **客户端估算**：仅处理 Subsonic 正式 `scrobble`，按歌曲时长估算；有原始时间戳时按用户、客户端、歌曲和毫秒时间戳去重，没有时间戳时每次提交独立计算。正在播放通知、获取播放地址、预加载和下载本身不计时。缺失歌曲时长时仅计次数，并在页面提示；未上报播放的客户端无法统计。早于统计启用时间的记录不纳入统计。

统计保存歌曲与歌手快照，元数据清理不影响排行；随现有 SQLite 完整备份保存，删除用户会同步删除其统计。页面可见时每 30 秒刷新。

「收听趋势」可通过点击图表／日历、日期选择框、前后一天按钮或滑块切换日期，鼠标移过不会改选。支持一键回到今天，详情显示完整日期、星期及当天的合计／实听／估算时长。刷新保留选中日期和日历滚动位置；编辑日期或拖动滑块期间暂缓自动刷新。

## 配置

启动项通过环境变量（优先）或 `data/config.yaml`（见 `config.example.yaml`）：

| 环境变量 | 说明 | 默认 |
|---|---|---|
| `LXSC_LISTEN` / `LXSC_PORT` | 监听地址 | `:8080` |
| `LXSC_DATA_DIR` | 数据目录（数据库、密钥、`sources/`） | `./data`（Docker 为 `/data`） |
| `LXSC_ADMIN_USER` / `LXSC_ADMIN_PASSWORD` | 首次启动创建的管理员 | 程序默认 `admin` / `admin`；Compose 要求显式设置密码 |
| `LXSC_PROXY` | 上游代理 `http://` 或 `socks5://` | 空 |
| `LXSC_SDK_WORKERS` | 搜索/歌词 SDK 的 JS 线程数 | `2` |
| `LXSC_LOG_LEVEL` | `debug` / `info` / `warn` / `error` | `info` |
| `LXSC_SECRET_KEY` | 用户口令加密密钥（留空自动生成到 `data/secret.key`） | 空 |

运行时设置（搜索平台顺序、播放/封面模式、缓存时间等）在管理页「设置」中修改；榜单展示在「歌单 → 自定义榜单」中配置，保存后立即生效。

### 榜单展示

在歌单页展开「自定义榜单」面板，由管理员统一配置，对所有用户的在线音乐目录和 Subsonic 歌单列表生效：

- 保留展示总开关和平台顺序；展开一个平台后，可选择「全部榜单」或「自定义」，自定义支持逐项勾选、全选和清空。
- 默认保持全部展示，并自动包含平台后续新增的榜单；自定义只展示已选榜单，清空该平台选择后隐藏其目录入口。
- 关闭总开关、暂时移除平台、榜单改名或平台加载失败均不会清除已有选择。暂时未返回的已选榜单会保留并标记，也可手动取消。
- 点击「保存榜单设置」后，服务端后续请求使用新选择；客户端需要刷新列表。榜单设置单独保存，不覆盖系统设置或当前歌单的草稿。原有榜单深链仍可访问，榜单保持只读。

管理员候选接口为 `GET /api/admin/boards?source=wy`，返回该平台未经展示筛选的完整 `Board` 数组（`source`、`bangid`、`name`、上游 `id`），不加载歌曲。保存时以规范化后的 `bangid` 匹配，不使用名称或上游 `id`。

`GET/PUT /api/admin/settings` 的 `boardSelections` 是“平台 → 榜单 ID 数组”的对象，例如 `{"wy":["3778678"],"tx":[]}`。缺少某平台键表示该平台展示全部，空数组表示不展示任何榜单；更新请求省略整个字段时保留原值，提供时整体替换，`{}` 恢复全部平台的默认模式。选择随现有设置持久化和备份，无需数据库迁移。

### 播放方式

管理员可在「设置 → 播放与缓存 → 播放方式」中选择：

- **302 重定向到音源直链**（`redirect`，默认）：由客户端直连音源；保留 Subsonic 客户端通过 `proxy=1` 请求服务器转发的兼容行为。
- **强制 302（禁止服务器转发）**（`force_redirect`）：网页播放、Subsonic 播放和下载均只返回音源直链，即使客户端通过查询参数或 POST 表单传入 `proxy=1` 也不能开启代理。取链失败返回错误，直连失败不回退到服务器转发。
- **服务端代理转发**（`proxy`）：由服务器拉取并转发音频。

修改对后续播放与下载请求生效，不中断已开始的传输。强制 302 只禁止音频转发；搜索、取链仍在服务器执行，封面方式由独立设置控制。

### 缓存与播放恢复

- **直链缓存**提供「不缓存、1 天、1 周、1 个月（30 天）、永久」五档，默认 1 周。管理接口 `urlCacheTTL` 对应 `0 / 86400 / 604800 / 2592000 / -1`，其他值返回 400；升级时旧版非预设秒数统一迁移为 1 周。搜索缓存仍按非负整数秒配置，默认 600 秒。即使关闭缓存，同时进行的相同取链请求仍会合并。
- 直链缓存在独立的 `data/url-cache.db` 中保存，按歌曲和请求音质区分，并保留可信的内部音源脚本 ID；重启后继续复用，读取或重启不会延长过期时间。「永久」不按时间过期，仍受链接失效、音源变更和最多 2000 条的最近最少使用淘汰约束。升级时无法确认脚本身份的旧格式直链缓存会重建，不修改用户、歌单等业务数据；缓存数据库故障时退化为内存缓存。
- 修改缓存档位只清空对应缓存；修改服务器名称等无关设置不清缓存。音源新增、启停、重载、删除、更新脚本或调整优先级后，内存与持久直链缓存同时失效；启动时也会核对音源和代理配置。已开始传输的音频不受影响。
- **302 播放与下载**对首次取链、缓存、刷新及备选链接都用 `GET`、`Range: bytes=0-0` 和 `Accept-Encoding: identity` 轻量校验响应头；每次校验最多等待 3 秒，收到头后立即关闭，不读取或转发整首音频。同一解析版本的并发校验会合并；`200/206` 才作为已验证候选返回 302。
- **媒体失败后换源**：`403/404/410` 对每个脚本最多重新取链一次，新链接仍失败或刷新解析失败时跳过该脚本；网络错误、超时、无效地址及其他非 `200/206` 响应可直接尝试下一个候选（`416` 除外）。保持音源优先级与源内音质降级规则，按真实脚本 ID 而不是脚本名或平台代号区分；只在本次请求内排除失败源，不全局拉黑。成功替代会缓存，迟到旧请求不能覆盖更新的缓存。
- **网络不确定与明确失败不同**：302 模式优先返回验证成功的备选；如果没有已验证备选，仍可保留一个未被明确拒绝的直链，让客户端按自己的网络尝试。不同脚本返回同一地址时，已观察到的明确拒绝不能被后续网络错误掩盖。全部已尝试候选明确失败则返回错误，不继续 302 到坏链接。
- 聚合搜索保持平台交错顺序；部分平台失败时返回成功平台的结果，但不缓存残缺结果。只有相同候选集合的取链会合并，单个客户端取消不影响其他等待者；无人等待时取消共享请求。
- **代理播放与下载**也在提交响应前按上述规则限次换源，每次获取响应头最多等待 8 秒；保留 Range/If-Range，媒体类型使用最终音质。`416` 及 Content-Range 按媒体协议透传，不换源；客户端取消或开始传输后的中断不会重新播放，也不追加协议错误。
- 每次播放最多选择优先级最高的 3 个候选脚本，最多 6 次媒体尝试；元数据、取链、校验和恢复共用 45 秒准备预算，HTTP 重定向仍可能产生后续请求。成功取得代理响应头后，不用此预算截断整首音频，客户端取消仍会停止传输。普通 **302 模式**（未请求代理时）与**强制 302 模式**均只校验后交付直链，绝不因失败自动代理。服务端看不到音流客户端跳转后的失败，响应头校验也不能证明客户端网络、内容或解码一定正常。
- **网页播放器同样遵循播放方式设置**，直连失败不会自动改走代理。HTTPS 页面访问 HTTP 音源、防盗链或浏览器格式限制可能导致直连失败，此时可重试、降低音质，或由管理员检查音源与播放方式。网页音频通过现有 HttpOnly 登录 Cookie 认证，不需要把 Subsonic 密码/API Key 放进地址，也不会扩大 `/rest` 的认证范围。

### 网页接口兼容性

`/api/app` 使用网页登录会话，普通用户与管理员遵循原有歌单权限：

- `POST /search`：保持 `{query,sources}` 请求及歌曲数组响应，歌曲对象新增 `duration`（秒，未知为 0）。
- `GET /stream?id=…&quality=…`：网页播放；音质可省略，省略时使用用户默认值，不能用 `proxy` 参数覆盖服务器设置。
- `POST /listening/progress`：接收 `{userId,sessionId,trackId,startedAt,days}`；`startedAt` 为毫秒时间戳，`days` 为北京时间日期到累计毫秒数的映射。`userId` 必须匹配当前登录账号，会话绑定歌曲与开始时间，服务端事务合并每日最大值。
- `GET /listening/stats?days=30&scope=me`：返回实听／估算汇总、每日数据和歌曲排行；`days` 支持 7、30、90、365。仅管理员可指定 `scope=all` 或 `scope=user&userId=…`，全站视图额外返回用户排行。时长字段单位为毫秒。
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

听歌统计验收还包括真实 WAV 计时、暂停与拖动排除、客户端去重、管理员筛选和账号隔离；桌面／平板／手机双主题截图使用测试拦截的示例数据，与真实计时验收分开。

## 限时安全调试

音流无法播放时，可由管理员在「限时调试」页创建临时凭据，私下提供当前部署地址和“复制分享说明”给受信任的排查者。默认 `read` 权限、15 分钟，可选 1/6/24 小时；固定绝对到期、不可续期，最长 24 小时。密钥仅显示一次、内存仅保存摘要、不落盘/备份，重启全部失效；离开页面或退出清空展示，用完请撤销。公网必须 HTTPS，分享不等于公开访问。

- `/api/debug/status` 和 `/api/debug/events` 仅返回白名单状态和至多 200 条固定字段诊断，不读取原始日志或导出用户、歌单、统计、脚本、配置、环境、备份及代理口令。
- 可选 `probe` 权限允许 `POST /api/debug/probe`，只接受歌曲 ID + 音质；会向音源发请求和少量流量，并可能填充现有缓存，不记录播放/收听统计。专用无代理传输逐跳验证公网 HTTP(S)/80/443 和 DNS/IP，防止 SSRF，只取 Range 响应头即关闭正文。
- 只接受 Authorization Bearer，不可用于 `/rest`、管理或普通应用 API。管理创建/列举/撤销仍要求当前管理员网页登录；创建/撤销有同源 Origin + 自定义头校验，不信任转发头，反代必须保留并验证 Host。这两组接口不开放 CORS、响应 no-store。
- 服务器 `status=0` 表示该阶段尚无 HTTP 响应，结合 `dns/timeout/connect/tls/cancelled` 等安全分类排查；服务器拿到 200/206 也不能证明音流客户端可播，不会改变强制302禁代理契约。

接口示例、HTTP 状态、权限/限频/并发/取消边界、反代要求和排查流程见 [docs/debug-api.md](docs/debug-api.md)。自动化测试与浏览器验收均只使用临时数据库和合成音源，不接触生产数据。

## 完整备份与 WebDAV

管理员可在统一控制台的「备份」页面管理完整迁移备份：

- **本地导出**：生成 `.lxsc-backup` 文件，包含 SQLite 一致性快照、用户/歌单/设置/音源、有效迁移密钥、`config.yaml` 与 `data/sources/`。可选填写独立备份密码进行 Age scrypt 加密。
- **本地导入**：上传备份后先检查格式、清单校验和、路径和 SQLite 完整性，再暂存恢复内容；当前服务不会立即被覆盖。
- **恢复生效**：导入成功后执行 `docker compose restart`。启动时会先保存最近一个本地回滚副本，再应用恢复；目标实例的监听地址、数据目录和加密密钥保持不变，用户密码会安全地重新加密到目标密钥。
- **WebDAV**：支持 Basic Auth/应用密码、连接测试、立即上传、远程列表、下载、恢复和删除。WebDAV 密码及远程备份加密密码均加密存储。
- **自动备份**：支持每日或每周按服务器时区执行；成功上传后默认只保留最近 3 份，设置为 `0` 可关闭自动清理。上传失败不会删除旧备份。

完整备份包含可用于迁移用户口令的敏感密钥。未设置备份密码时，请只存放在受信任位置；远程 WebDAV 推荐使用 HTTPS。

直链缓存数据库 `data/url-cache.db` 及其 WAL、SHM 和故障标记文件不进入本地导出、WebDAV、自动备份或恢复回滚副本；缓存档位设置仍随主数据库备份。暂存恢复不影响当前缓存，恢复生效及恢复回滚时清空直链缓存，后续播放按需重新取链。

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
