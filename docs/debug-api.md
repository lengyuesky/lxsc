# 限时安全调试 API

用于管理员私下邀请受信任的排查者诊断“音流无法播放”。**分享不等于公开访问**：不要把凭据放在公开 issue、截图、URL、命令历史或日志里；公网务必使用 HTTPS。调试凭据不是网页登录密码或 Subsonic API key，不能播放、管理配置、下载脚本或导出用户内容。

## 使用流程

1. 管理员登录当前部署的统一控制台，打开 **限时调试**。
2. 默认创建 `read`、15 分钟的临时凭据；也可选 1 小时、6 小时、24 小时。需要服务器主动请求音源时，单独勾选 `probe`。UI 明确说明该操作有网络请求、少量流量与缓存副作用。
3. 密钥只在创建响应/当前页面显示一次。复制“分享说明”，私下提供给排查者；说明含当前部署地址、`Bearer` 凭据、绝对到期时间、scope 和 API 入口。剪贴板失败时使用只读文本框手工复制。
4. 离开该页、退出登录或关闭页面会清空密钥展示；浏览器不将密钥写入 localStorage、sessionStorage 或 URL。清除展示不等于撤销；若创建期间离开页面，已创建凭据可能仍有效，可在列表撤销并重建。
5. 用完立即撤销。列表仅显示 ID、权限、创建/绝对到期/最近使用时间、调用数和撤销状态，不会再次显示明文或摘要。任何当前有效管理员可查看/撤销，普通用户前后端均禁止。
6. 到期不能续期，只能新建；删除/降权创建者会使其凭据失效，重新升权也不恢复已撤销凭据。进程重启后所有凭据丢失，备份不会包含这些凭据。

密钥使用 `crypto/rand` 生成 256 位随机数，内存仅保存 SHA-256 摘要、创建者关联、到期与计数等元数据，不落盘。最多 16 个有效凭据，包含已失效记录的表最多 64 项；过期记录保留最多约 1 小时（惰性清理/容量回收），管理审计最多 128 条，仅记录 ID、时间与固定操作类型。审计不是持久化的安全日志。

## 调试调用

所有 `/api/debug` 请求必须使用：

```http
Authorization: Bearer <仅使用刚创建的临时凭据>
```

只接受 `Authorization: Bearer`，不接受 query 参数、Cookie 登录或普通 Subsonic key。带 query 的调试请求直接拒绝。下列示例不包含真实凭据；建议用交互式输入暂存环境变量，避免将真实值写入命令历史：

```bash
read -r -p 'HTTPS 部署地址（不含末尾斜线）: ' DEBUG_BASE
read -r -s -p '临时调试凭据: ' DEBUG_TOKEN; printf '\n'
curl --fail-with-body --silent --show-error \
  -H "Authorization: Bearer $DEBUG_TOKEN" "$DEBUG_BASE/api/debug/status"
curl --fail-with-body --silent --show-error \
  -H "Authorization: Bearer $DEBUG_TOKEN" "$DEBUG_BASE/api/debug/events"
# 仅有 probe 权限时；填写用户明确要排查的歌曲ID，不传任意URL。
curl --fail-with-body --silent --show-error \
  -H "Authorization: Bearer $DEBUG_TOKEN" -H 'Content-Type: application/json' \
  --data '{"trackId":"tr-wy-123456","quality":"320k"}' \
  "$DEBUG_BASE/api/debug/probe"
unset DEBUG_TOKEN DEBUG_BASE
```

不要开启 `curl -v` 或将含请求头的输出公开。命令参数可能被同机进程观察，应只在受信任的本机终端使用。

| 方法/入口 | 权限 | 响应 |
| --- | --- | --- |
| `GET /api/debug/status` | read | 白名单状态对象：`version`、`uptimeSeconds`、`streamMode`、`urlCacheMode`、平台代号数组、音源健康计数 |
| `GET /api/debug/events` | read | `{ "events": [...] }`，最近至多 200 条结构化播放/探测事件 |
| `POST /api/debug/probe` | read + probe | `{ "events": [...], "mediaHeaders": {...}, "perspective": "server_only", "cacheSideEffects": true }`，至多 3 个阶段 |

`trackId` 仅接受 `tr-(wy|tx|kw|kg|mg)-` 后接 1–128 个 ASCII 字母、数字、下划线或连字符；不接受自定义 URL/脚本/headers。`quality` 必填，支持当前音质枚举 `128k`、`192k`、`320k`、`flac`、`flac24bit`、`hires`、`master`、`atmos`、`atmos_plus`、`dolby`，实际解析可能按歌曲/音源能力降档。

### 结构化事件与 status=0

每条事件仅含 `time`、`stage`、受限 `trackId`、平台代号、音质、模式、是否命中缓存、HTTP `status`、安全 `error` 分类和 `elapsedMs`。阶段包括：

- `metadata`：元数据查询；`resolve` / `refresh`：首次取链或过期刷新；`source_fallback`：排除失败脚本后的备选取链。
- `url_check`：新解析、刷新或备选直链的最小 Range 校验；`cache_check`：缓存直链 Range 校验；`redirect`：交付 302 给客户端。
- `proxy_response`：代理取得上游响应；`proxy_copy`：代理传输结束/中断。
- `probe`：独立主动探测 HTTP 响应头。

`status=0` **不是 HTTP 状态码**：取链阶段本来没有 HTTP 响应，或请求在取得响应之前失败。结合阶段与 `error` 判断：

| error | 含义/排查方向 |
| --- | --- |
| `none` | 阶段无已知错误；不等于客户端可播放 |
| `dns` | DNS 解析失败：检查服务器解析器、平台/CDN域名可达性 |
| `timeout` | 阶段/连接/响应头超时：检查服务器出口网络及音源响应速度 |
| `connect` | 建连失败/拒绝/无路由：检查出口防火墙、网络路由 |
| `tls` | 可识别的证书/主机名/握手格式失败：检查时间、证书链、TLS链路；不要通过关闭证书验证“修复” |
| `cancelled` | 客户端取消、撤销、创建者失效等引起的取消 |
| `blocked_target` | 主动探测拒绝不安全协议、端口、IP或DNS结果 |
| `redirect_limit` | 主动探测重定向超过 3 跳 |
| `non_audio` | 上游虽返回 200/206，但类型明确为文本、图片或 JSON/XML，不能作为音频；普通播放会尝试其他音源，主动探测只报告 |
| `upstream_error` | 无法安全细分的上游错误，不输出原始错误字符串 |

例如 `cache_check status=0 error=dns` 后出现 `redirect status=302`，表示服务器可能在尝试备选后仍无法验证音频，但保留了未被明确拒绝的客户端直连机会，并不表示服务端成功请求了音频。`403/404/410` 会触发每源至多一次刷新，仍失败则尝试其他脚本；普通播放新增的 `source_fallback` 只表示备选取链阶段，不暴露脚本 ID 或名称。平台 `tx` 等代号也不是音源脚本身份。`200/206` 只证明服务端拿到了这些响应头，可能仍是错误内容、编解码不支持、客户端网络/CORS/混合内容或签名绑定问题。

建议排查顺序：查看模式和音源健康 → 用户在自己的音流客户端重试指定歌曲 → 查结构化阶段/分类 → 必要时显式授权 probe 交叉检查。服务端探测无法观察音流客户端实际网络。不要索取用户全部歌单、原始日志或永久凭据。`force_redirect` 继续禁止客户端 `proxy=1` 覆盖，不会因诊断失败自动代理；独立 probe 只读取服务端响应头，不向客户端转发音频，也不会因媒体拒绝而自动刷新或执行整套播放切源。

### 主动探测的边界

- 沿用 Catalog 元数据/链接解析，可能填充内存元数据缓存以及现有内存/持久化直链缓存、更新缓存使用时间；不调用播放记录、收听统计、歌曲持久化或修改配置。不会强制刷新现有可用缓存。
- 现有管理员安装的音源脚本仍有自己的外网能力；调试接口不上传、修改、选择或执行任意脚本，安全音频传输限制**不代表沙箱化已有脚本的所有请求**。
- 专用 HTTP 传输仅允许公网 `http/https`、80/443 端口，禁止 userinfo、私网、loopback、link-local、metadata、CGNAT、保留/文档/隧道地址、IPv4 映射 IPv6 等。DNS 所有结果先校验，再绑定已检查 IP 直连，混合公私网也拒绝；每跳重定向重复校验，至多 3 跳。
- 不读取环境或配置代理；正常校验 TLS 证书。与实际播放使用的网络路径、UA/Referer可能不同，因此失败不证明实际播放器失败。
- 固定 `Range: bytes=0-0`、`Accept-Encoding: identity`，收到响应头立即关闭正文，不读取整首音频。上游可能忽略 Range 或在关闭前发送少量额外数据，不能保证实际仅消耗 1 字节。
- 只返回经过格式/枚举校验的媒体类型、内容长度、Content-Range、Accept-Ranges；不返回签名 URL、路径、任意上游正文、ETag、Cookie、Location 或其他 headers。单跳响应头最多 16 KiB；DNS 答案最多 32 个。
- 请求 JSON 最大 2048 字节，拒绝未知/重复字段、null、非对象或尾随 JSON。每次调试请求总限时 20 秒且不晚于凭据绝对到期（包含创建者数据库校验/连接池等待、慢正文读取和响应刷新），HTTP 探测限时 12 秒，建连/TLS 5 秒、响应头 8 秒。到期/撤销取消在途工作；管理删除/降权立即撤销，其他数据库变更在新请求时及在途每 250ms 校验。Catalog 与正常播放共享的在途工作只在最后一个等待者离开后取消，不干扰其他正常播放。
- 敏感 HTTP/1 连接使用响应后关闭，不复用连接；无认证、权限不足、方法/格式错误和限流等提前拒绝也不等待客户端补齐未读正文，不影响普通播放连接。SDK/音源调用通过 Promise、定时器及 HTTP 回调传递调用归属，取消仅结束该调用的网络/定时器，不卸载音源或按 Worker 全局取消。
- 每凭据每分钟最多 30 次已认证调用（含最终失败/无效参数的调用）；全局同时最多 8 个调试请求、2 个主动探测。超限 `429`，附 `Retry-After: 60`（凭据限频/数量）或 `1`（并发）；不会排队积累无限任务。拒绝的无效凭据不进入审计。

## 管理接口

位于 `/api/admin/debug-tokens`，必须是**当前有效管理员网页登录 Cookie**；调试 Bearer 不适用。仅管理员可列举/创建/撤销，不提供续期或重新取密钥接口。

```http
GET /api/admin/debug-tokens
# 响应 { "tokens": [TokenView...], "audit": [{ "time": "...", "id": "...", "action": "created|used|revoked|owner_invalid" }] }

POST /api/admin/debug-tokens
Origin: https://your-deployment.example
X-LXSC-Debug-Management: 1
Content-Type: application/json

{"ttlSeconds":900,"scopes":["read","probe"]}
# 201: { "token": "<仅此一次>", "credential": { "id": "...", "scopes": [...], ... } }
# {} 等同默认 read + 900秒。ttlSeconds仅允许900/3600/21600/86400。

DELETE /api/admin/debug-tokens/{id}
Origin: https://your-deployment.example
X-LXSC-Debug-Management: 1
# 无请求体；200: { "ok": true }，重复撤销已存在记录同样成功。
```

创建/撤销必须有合法 Origin 和固定非简单请求头 `X-LXSC-Debug-Management: 1`；Origin 不允许 userinfo、路径、query 或 fragment，完整 authority（含显式端口）必须匹配请求 Host；不信任任意 `X-Forwarded-*`。若有 `Sec-Fetch-Site`，必须为 `same-origin`。直接 TLS 只接受 https Origin；明文后端允许同 authority 的 http/https Origin，以兼容 HTTPS 反代。**反代必须保留并验证 Host，后端不能独立确认外部 scheme，公网必须 HTTPS**。不提供无 Origin/Referer 降级，也不新增可信转发配置。

调试与凭据管理接口不开放跨域预检或 CORS（不影响既有普通 API 的 CORS），所有安全响应 `Cache-Control: no-store`。浏览器跨源请求不能使用这些接口；无 Origin 的服务器端 Bearer 调用可用。生产反向代理还应关闭这两组路径的缓存与请求体/Authorization日志，不要额外注入宽松 CORS。

## HTTP 状态

- `200`：读取/撤销成功；probe 即使上游失败仍返回安全诊断阶段，需检查 `events[].error/status`。
- `201`：创建成功，密钥只返回一次。
- `400`：字段、类型、TTL、歌曲 ID、音质、JSON/大小等不合法。
- `401`：缺少或无效的对应认证；调试凭据过期、已撤销、进程重启、创建者失效也属于无效凭据。
- `403`：非管理员、缺 probe 权限、跨源/CSRF校验失败或禁止预检。
- `404`：凭据 ID/入口不存在。
- `405`：不支持的方法。
- `408`：探测期间取消/超时/过期/撤销，不返回已取得的诊断内容；慢请求体/慢客户端同时受网络读写截止控制，可能直接断开连接而无法收到响应。
- `429`：数量、限频或并发上限，遵循 `Retry-After`。
- `503`：状态依赖暂不可读或安全随机凭据暂无法创建；不返回原始数据库/系统错误。

本 API 不导出完整 settings、用户、歌单、听歌统计、原始日志、脚本、环境、备份或代理口令。原有 `internal/logbuf` 是自由文本，仅供已有管理员日志页面使用，调试接口从不读取它，也不以正则替换原始日志作为安全边界。

## 隔离验证

测试只用临时 SQLite、合成音源、注入 DNS/拨号的本地 HTTP/TLS 服务，不读取生产目录或真实凭据：

```bash
go test ./...
go test -race ./...
go vet ./...
node --test tests/web/*.test.cjs
git diff --check
# 有现成 Playwright/Chromium 时，路径按本机已有安装填写；不要求安装新工具。
LXSC_PLAYWRIGHT_MODULE=/path/to/node_modules/playwright \
LXSC_CHROMIUM_PATH=/path/to/chrome node tests/web/browser.cjs
```

恢复回归还覆盖真实 TCP 提前拒绝后响应可读且连接结束、唯一数据库连接被占用时第9个请求拒绝/撤销/到期/默认20秒期限，以及 JS 取链/SDK元数据上游取消和正常播放共享取链不受影响。

覆盖认证隔离、管理员/CSRF/TTL、撤销/到期/创建者失效的在途取消、数量/限频/并发、恶意错误脱敏、DNS绑定与重定向/私网/TLS、无播放持久化副作用、实际 h_media 事件及强制302、前端一次显示/复制失败/撤销/清理。切源回归另覆盖同名不同 ID 脚本、首次/缓存/刷新校验、坏源耗尽、总预算与长音频、候选集合并发隔离、持久缓存身份，以及 probe 收到403仍仅交付原三阶段、不自动切源。浏览器 fixture 的音频在 localhost，主动探测预期返回 `blocked_target`，不会为测试放松生产 SSRF规则。

## 诊断版本 2：箭头音乐与 AI 排查

`GET /api/debug/status` 的 `diagnosticsVersion: 2` 表示支持本节功能。`GET /api/debug/capabilities` 返回机器可读的鉴权说明、端点、请求示例、限制和排查步骤，需要 `read` 权限。管理页复制的分享说明包含这一入口，可直接私下交给 AI。

箭头音乐仍使用自己的普通账号，以 **Subsonic** 类型连接服务端；临时调试 Bearer 仅供 AI 调用 `/api/debug`，不能填入播放器，也不能读取原始日志或管理数据。

### 观察真实客户端请求

让用户先在箭头音乐中打开问题榜单、重试播放或进入歌曲的歌词页，然后读取 `/api/debug/events`。`stage=client_response` 覆盖 `getPlaylists`、`getPlaylist`、`getMusicDirectory`、`getLyrics`、`getLyricsBySongId`、`getSong`、`getAlbum`、`stream` 和 `download`，包括认证失败的协议响应。

- `client`：根据客户端声明的 `c` 参数归类为 `amcfy`、`stream_music`、`other`、`unknown`；仅用于诊断，不作为可信身份。原始客户端字符串不输出。
- `endpoint`、`method`、`format`：固定枚举的接口名、`GET/POST/HEAD` 及 `json/xml/jsonp/binary`。媒体成功响应（含 302）标记 `binary`，协议错误保留原格式。`stream/download` 的 `HEAD` 是客户端链接预检，不能当作真实播放；它与独立 `probe` 也不同。
- `result`：`ok/empty/failed/unavailable`；`protocolCode` 单独表示 Subsonic 错误码。HTTP 200 不代表协议成功。
- `count`：实际响应中的列表条目数（歌单详情为实际歌曲数，非摘要的待加载标记）。`lines`：歌词行数；`synced`：结构化歌词是否含同步轨。字段不存在表示不适用，零表示确实返回空。
- `trackId`、`boardId`：仅接受固定平台前缀和有界 ID；歌单列表事件还提供最多 10 个公开在线榜单 `boardIds`，便于 AI 在客户端未请求详情时定向探测。不输出自定义歌单 ID、歌曲/歌手名称、歌词正文、用户名、认证字段或任意请求头。

事件只表示服务端生成的协议响应，不证明客户端收到了全部字节或解析成功。没有事件可能是客户端未请求、离线缓存、反向代理未转发、窗口已被最近 200 条事件覆盖或部署版本不支持，不能仅凭事件缺失断言客户端故障。

媒体接口支持带或不带 `.view` 的 `HEAD`，沿用普通播放器认证和音质。普通 302／代理模式在服务端用 `GET Range: bytes=0-0` 验证上游，响应头到达后立即关闭正文，并在本站返回 HTTP 200 媒体头；不会让客户端继续跳转后向只支持 GET 的音源发送 HEAD。206 探测响应的完整大小取自合法的 `Content-Range`，不把一字节探测长度当成文件大小；未知大小不编造。HEAD 不读取或转发正文、不占用整首音频传输额度，也不记为播放；仍受准备请求并发和期限限制。真实 GET/POST 继续遵循原播放方式，强制 302 模式的 HEAD 也保持重定向。

HEAD 无法携带协议正文，失败时用 HTTP 400/401/403/404/429/502/503 表达参数、认证、权限、歌曲、限频或上游问题，协议码保留在 `client_response.protocolCode`。上游失败不能被包装成 HTTP 200 或未经验证的媒体头。其他 Subsonic 接口不新增 HEAD 操作。旧版仅注册 GET/POST，媒体 HEAD 会在进入诊断前返回 405；仅补上路由但继续向上游发 HEAD 或重定向，也可能出现“GET 能播放、HEAD 仍失败”，需要分别核对两条链路。

链接校验与代理准备只检查响应头，不读取错误页正文。明确的非音频类型会以 `non_audio` 记入诊断、丢弃对应失败缓存并限次换源；强制 302 不会自动代理。主动探测使用同一类型判断，但只报告原三阶段，不执行刷新或换源。未声明类型或通用二进制类型继续兼容，返回 200/206 或空的 `mediaHeaders` 仍不能证明内容或客户端解码正常。

### 定向主动探测

需要现有的 `read + probe` 权限，共用每凭据每分钟 30 次、全局最多 2 个并发探测、请求总计 20 秒、撤销和到期取消等限制。只用用户指定或事件提供的 ID，不批量扫描：

| 方法/入口 | JSON 请求 | 返回 |
| --- | --- | --- |
| `POST /api/debug/probe/board` | `{"boardId":"lb-wy-19723756"}` | `board_probe` 事件、完整榜单歌曲数量、最多 3 个合法歌曲 ID 供后续定向歌词探测 |
| `POST /api/debug/probe/lyrics` | `{"trackId":"tr-wy-123456"}` | `lyrics_probe` 事件、歌词行数、同步状态或安全错误分类，无歌词正文 |

只返回 `events`、`sampleTrackIds`、`perspective: server_only`、`cacheSideEffects: true`。JSON 拒绝未知/重复字段、URL、非法 ID、null 和尾随内容。榜单探测沿用现有目录缓存；歌词探测优先 SDK、必要时回退音源。两者可能填充短期元数据和歌词缓存，不写入资料库、不记录播放、不修改配置，不执行音频流请求。

AI 应先比较真实 `client_response`：返回空或失败时，再对同一 ID 探测上游；真实响应有歌曲/歌词而客户端不显示时，继续检查客户端版本、连接类型、响应格式与缓存。主动探测不是客户端模拟，不能单独证明兼容问题已修复。部署升级会让旧临时凭据失效，需重新创建；用完只能由管理员在控制台撤销。
