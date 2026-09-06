# 许可归档索引

本目录与根目录 `LICENSE`、`NOTICE`、`THIRD_PARTY_NOTICES.md` 一起随应用分发。镜像位置为 `/usr/share/licenses/lxsc/`，不是将所有组件统一重新授权。

- `manifest.json` 记录固定版本/提交、原文件路径、归档位置与 SHA-256；README 协议摘录另记完整上游 README 摘要及起止行，摘录中的文字未改写。
- `vendor-provenance.json` 保留 87 个 vendor 文件的两组核对摘要，以及导入版本未知这一边界。新增的补记标记之外，SDK 内容没有改变。
- `GO_SOURCE_NOTICES.txt` 汇总 Linux 双架构运行包源码内的原始版权/许可注释。四个涉及 GNU 头文件的单元另以 `.go.txt` 完整归档，避免只截取声明；它们不参与编译。
- 归档也保守保留相关模块中的部分额外法律文件（例如 pprof 的 svgpan、memory 的图标声明），不意味着那些额外代码/图标进入了最终应用。
- Alpine 系统组件另见根目录第三方说明和镜像中的 `ALPINE_PACKAGES.txt`（完整 APK 原始包记录）。`alpine/` 补充保存 GPL-2.0 与初始核对的 OpenSSL、musl、zlib 原文；这里不是基础系统的完整源码/许可审计。

## 组件与原文

下表的许可名称是检索索引，适用范围和条件以完整原文为准。源码地址、全部附属法律文件与摘要见 `manifest.json`。

| 组件 | 核对版本 | 许可索引 | 原文入口 |
|---|---|---|---|
| `lxsc-license` | `Apache-2.0` | Apache-2.0（仅本项目原创部分） | [原文](../LICENSE) |
| `lxserver-reference` | `64f4bf24143d26c2a03b86bb0abd16919189b7a8` | Apache-2.0 及上游 README 补充协议 | [原文](upstream/lxserver-reference/LICENSE) |
| `lxserver` | `5c220b978ac1864d0a367930e579f009dfe7f933` | Apache-2.0 及上游 README 补充协议 | [原文](upstream/lxserver/LICENSE) |
| `lx-music-desktop` | `9c364b482e5621a1d38b50e8610d2fb974457e6e` | Apache-2.0 及上游 README 补充协议 | [原文](upstream/lx-music-desktop/LICENSE) |
| `NeteaseCloudMusicApi-reference` | `4.32.0` | MIT | [原文](upstream/NeteaseCloudMusicApi/LICENSE) |
| `wp_MusicApi-reference` | `bf9307dd138dc8ac6c4f7de29361209d4f5b665f` | Apache-2.0 | [原文](upstream/wp_MusicApi/LICENSE) |
| `alpine-license-references` | `2026-09-06` | GPL-2.0、OpenSSL 3.5.7、musl 1.2.6、zlib 1.3.2；具体以实际包记录为准 | [原文目录](alpine/) |
| `GNU-LGPL-2.1` | `2.1` | LGPL-2.1-or-later（对应头文件声明允许后续版本） | [原文](texts/LGPL-2.1.txt) |
| `go-stdlib` | `1.27.0` | BSD-3-Clause 及标准库内联第三方条款 | [原文](go-stdlib/go1.27.0/LICENSE) |
| `filippo.io/age` | `v1.2.1` | BSD-3-Clause；bech32 部分 MIT | [原文](go/filippo.io/age@v1.2.1/AUTHORS) |
| `github.com/dlclark/regexp2/v2` | `v2.5.2` | MIT | [原文](go/github.com/dlclark/regexp2/v2@v2.5.2/LICENSE) |
| `github.com/dop251/base64dec` | `v0.0.0-20231022112746-c6c9f9a96217` | BSD-3-Clause | [原文](go/github.com/dop251/base64dec@v0.0.0-20231022112746-c6c9f9a96217/LICENSE) |
| `github.com/dop251/goja` | `v0.0.0-20260901132549-43234fa61381` | MIT；V8 部分 BSD-3-Clause；Lucent 部分见原文 | [原文](go/github.com/dop251/goja@v0.0.0-20260901132549-43234fa61381/LICENSE) |
| `github.com/dop251/goja_nodejs` | `v0.0.0-20260212111938-1f56ff5bcf14` | MIT | [原文](go/github.com/dop251/goja_nodejs@v0.0.0-20260212111938-1f56ff5bcf14/LICENSE) |
| `github.com/dustin/go-humanize` | `v1.0.1` | MIT | [原文](go/github.com/dustin/go-humanize@v1.0.1/LICENSE) |
| `github.com/go-chi/chi/v5` | `v5.3.2` | MIT | [原文](go/github.com/go-chi/chi/v5@v5.3.2/LICENSE) |
| `github.com/go-sourcemap/sourcemap` | `v2.1.4+incompatible` | BSD-2-Clause | [原文](go/github.com/go-sourcemap/sourcemap@v2.1.4+incompatible/LICENSE) |
| `github.com/google/pprof` | `v0.0.0-20260802141513-ef3492d7dac3` | Apache-2.0（附带 svgpan 的 BSD 原文） | [原文](go/github.com/google/pprof@v0.0.0-20260802141513-ef3492d7dac3/AUTHORS) |
| `github.com/google/uuid` | `v1.6.0` | BSD-3-Clause | [原文](go/github.com/google/uuid@v1.6.0/LICENSE) |
| `github.com/hashicorp/golang-lru/v2` | `v2.0.7` | MPL-2.0；链表部分 BSD-3-Clause | [原文](go/github.com/hashicorp/golang-lru/v2@v2.0.7/LICENSE) |
| `github.com/remyoudompheng/bigfft` | `v0.0.0-20230129092748-24d4a6f8daec` | BSD-3-Clause | [原文](go/github.com/remyoudompheng/bigfft@v0.0.0-20230129092748-24d4a6f8daec/LICENSE) |
| `golang.org/x/crypto` | `v0.55.0` | BSD-3-Clause 及 PATENTS | [原文](go/golang.org/x/crypto@v0.55.0/LICENSE) |
| `golang.org/x/net` | `v0.58.0` | BSD-3-Clause 及 PATENTS | [原文](go/golang.org/x/net@v0.58.0/LICENSE) |
| `golang.org/x/sys` | `v0.47.0` | BSD-3-Clause 及 PATENTS | [原文](go/golang.org/x/sys@v0.47.0/LICENSE) |
| `golang.org/x/text` | `v0.41.0` | BSD-3-Clause 及 PATENTS | [原文](go/golang.org/x/text@v0.41.0/LICENSE) |
| `gopkg.in/yaml.v3` | `v3.0.1` | MIT（libyaml 部分）及 Apache-2.0（其余部分），含 NOTICE | [原文](go/gopkg.in/yaml.v3@v3.0.1/LICENSE) |
| `modernc.org/libc` | `v1.75.6` | BSD-3-Clause；musl 等 MIT/BSD；GNU 头文件 LGPL-2.1-or-later；详见附带声明 | [原文](go/modernc.org/libc@v1.75.6/AUTHORS) |
| `modernc.org/mathutil` | `v1.7.1` | BSD-3-Clause | [原文](go/modernc.org/mathutil@v1.7.1/AUTHORS) |
| `modernc.org/memory` | `v1.12.1` | BSD-3-Clause（含 Go、mmap-go 原文） | [原文](go/modernc.org/memory@v1.12.1/AUTHORS) |
| `modernc.org/sqlite` | `v1.58.0` | BSD-3-Clause；SQLite 公共领域声明；sqlite-vec MIT | [原文](go/modernc.org/sqlite@v1.58.0/AUTHORS) |
| `base64-js` | `1.5.1` | MIT | [原文](npm/base64-js@1.5.1/LICENSE) |
| `buffer` | `6.0.3` | MIT | [原文](npm/buffer@6.0.3/LICENSE) |
| `ieee754` | `1.2.1` | BSD-3-Clause | [原文](npm/ieee754@1.2.1/LICENSE) |
| `esbuild` | `0.28.2` | MIT | [原文](npm/esbuild@0.28.2/LICENSE.md) |

## 维护方式

在仓库根目录用 Go 1.27.0、Node 26 和 Bun 1.3.14 安装锁定依赖、重建 JS 后运行：

```bash
node scripts/licenses.mjs --check
node --test tests/licenses.test.mjs
```

更新版本时须先审核依赖变化和原许可，修改 `manifest.json` 中的版本、源路径及许可索引；`node scripts/licenses.mjs --update` 只从已安装的精确版本源码更新副本、内联注释和摘要，新增/移除模块或原有上游归档的变化不会被自动批准。涉及 SDK 内容变更，还需更新逐文件来源证据与 `js-bridge/vendor/PATCHES.md`，不能抹去既有来源。

实际使用的包集合与 `go.mod` 的全部要求不必相同；此处针对 `CGO_ENABLED=0` 的 `./cmd/lxsc`、`linux/amd64` 与 `linux/arm64`。其他平台、CGO 或构建标签不在此核对范围内。测试与构建工具程序本身不作为应用运行依赖分发；esbuild 的 MIT 原文因其生成辅助代码而额外随附。
