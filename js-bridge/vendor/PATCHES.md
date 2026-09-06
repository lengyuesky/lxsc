# vendor 来源与修改记录

## 已核对来源，不等于最初导入版本

原始代码来自 [XCQ0607/lxserver](https://github.com/XCQ0607/lxserver) 的 `src/modules/utils`，其音乐 SDK 再上游为 [lyswhut/lx-music-desktop](https://github.com/lyswhut/lx-music-desktop)。`common/lyricUtils/` 另取自 lxserver 的歌词工具目录。

2026-09-06 核对了以下不可变提交：

- lxserver 参考版本：[`64f4bf24143d26c2a03b86bb0abd16919189b7a8`](https://github.com/XCQ0607/lxserver/tree/64f4bf24143d26c2a03b86bb0abd16919189b7a8)。已有干净克隆位于此提交，其 LICENSE/README 已与公开仓库同提交原文逐字节核对。87 个本地 JS/TS 文件中，65 个与该版本对应文件完全一致，22 个有下表所列差异。
- lxserver 当前核对版本：[`5c220b978ac1864d0a367930e579f009dfe7f933`](https://github.com/XCQ0607/lxserver/tree/5c220b978ac1864d0a367930e579f009dfe7f933)。57 个文件完全一致、30 个有差异；不能把上游后续变动都归为 lxsc 修改。
- lx-music-desktop 协议核对版本：[`9c364b482e5621a1d38b50e8610d2fb974457e6e`](https://github.com/lyswhut/lx-music-desktop/tree/9c364b482e5621a1d38b50e8610d2fb974457e6e)。这里只证明已核对其协议，不宣称 lxserver 最初使用的桌面版提交就是此版本。

最初导入的上游提交**未知**。本仓库首次受跟踪提交 `4658e093493a58a19edb4ca972101cddfc0dc552` 只证明文件当时已存在，不能恢复更早的导入过程。逐文件路径、内容 SHA-256、匹配结果见 [`licenses/vendor-provenance.json`](../../licenses/vendor-provenance.json)；本次补记的第一行标记不计入内容摘要。

`common/lyricUtils/kg.js`、`common/lyricUtils/util.ts` 在参考版本对应 `src/common/utils/lyricUtils/`，在当前核对版本对应 `src/utils/lyricUtils/`，内容均相同。

## 相对参考版本的修改

以下均为本次许可整理前已经存在的实现差异。本次仅补记文件头和文档，没有重新改写这些业务逻辑；原有来源、版权、许可注释均保留。

| 本地路径 | 已有差异 |
|---|---|
| `request.js` | 保留 needle 语义封装，移除 tunnel/代理选择等逻辑；请求与代理交给 Go 宿主。 |
| `musicSdk/api-source.js` | 改为存根，音频 URL 由宿主通过自定义音源脚本获取。 |
| `index.js` | 添加调用宿主哈希的 `toMD5`，供 bd 模块引用。 |
| `musicSdk/kw/lyric.js` | `RegExp.$1` 改为 `exec` 的捕获结果，兼容 goja。 |
| `musicSdk/mg/musicInfo.js` | 正则捕获改用 `exec`；修复 `Promise.all(Promise)`；补充歌手 ID。 |
| `musicSdk/mg/lyric.js` | resourceinfo 返回空或请求失败时，回退到歌曲已有的 lrcUrl/mrcUrl。 |
| `musicSdk/kg/index.js` | 暴露歌手、专辑目录模块。 |
| `musicSdk/kw/index.js`、`musicSdk/mg/index.js` | 暴露专辑目录模块。 |
| `musicSdk/kg/singer.js` | 从封面字段读取图片地址，不再对对象本身调用 `replaceAll`。 |
| `musicSdk/wy/extendDetail.js` | 无发行时间时跳过日期格式化。 |
| `musicSdk/kg/leaderboard.js`、`musicSdk/kg/musicInfo.js`、`musicSdk/kg/musicSearch.js` | 歌曲结果补充歌手 ID。 |
| `musicSdk/kw/leaderboard.js`、`musicSdk/kw/musicSearch.js` | 歌曲结果补充歌手 ID。 |
| `musicSdk/mg/musicSearch.js` | 歌曲结果补充歌手 ID。 |
| `musicSdk/tx/leaderboard.js`、`musicSdk/tx/musicSearch.js`、`musicSdk/tx/singer.js` | 歌曲结果补充歌手 ID。 |
| `musicSdk/wy/musicDetail.js`、`musicSdk/wy/musicSearch.js` | 歌曲结果补充歌手 ID。 |

原记录中的删减继续保留：未引入各平台 `api-test.js`、`temp/`、`kw/api-temp.js` 等未使用内容；上游目录内的说明文档也不是本地 SDK 源文件清单的一部分。

## 许可与再上游引用

两个上游的完整 LICENSE 和 README 适用协议节已归档到 [`licenses/upstream/`](../../licenses/upstream/)。lxserver LICENSE 末尾原有的 File Browser Contributors 声明未删改。两个 README 均包含明确的补充协议及其优先表述，不能只看 Apache 标签。

`musicSdk/wy/utils/crypto.js` 和 `musicSdk/wy/songList.js` 原有 NeteaseCloudMusicApi 来源注释、`musicSdk/kg/comment.js` 原有 wp_MusicApi 固定提交引用继续保留；相应许可副本、证据边界见 [`THIRD_PARTY_NOTICES.md`](../../THIRD_PARTY_NOTICES.md)。`kg/vendors/infSign.min.js` 按已核对 lxserver 文件保留，未找到它在该上游中另附的独立来源/许可说明，不声称已经完成其更深层权属审计。
