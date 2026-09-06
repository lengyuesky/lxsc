# 第三方许可、来源与再分发说明

## 授权范围

根目录 [LICENSE](LICENSE) 是标准 Apache License 2.0，适用于 lxsc 贡献者有权授权的原创代码及本项目所作的原创修改，**不重新授权第三方代码、音乐数据、用户提供的音源脚本或基础镜像**。第三方内容各自适用其原许可、版权声明、NOTICE 和其他适用条款。不存在把所有依赖统一改为 Apache-2.0 的授权。

本文件是随发行版提供的许可索引，不是完整权属/专利审计或法律意见，也不能代替履行原许可。使用者仍须具有访问、使用和分发有关音乐资源的权利；“用于学习”“24 小时内删除”或上游免责声明不当然免除法律责任。镜像元数据中的 `LicenseRef-LXSC-Third-Party` 指本文件列明的第三方条款集合，不是一份新增许可。

镜像将根 `LICENSE`、`NOTICE`、本文件、`licenses/` 和 `js-bridge/vendor/PATCHES.md` 原路径布局放在 `/usr/share/licenses/lxsc/`。从镜像抽出二进制、另制压缩包或转发 JS bundle 时，也应随附这些材料；内联注释不能替代完整的许可副本。

## 音乐 SDK 来源与上游补充协议

| 核对对象 | 固定核对版本 | 原文副本 |
|---|---|---|
| XCQ0607/lxserver，参考来源树 | `64f4bf24143d26c2a03b86bb0abd16919189b7a8` | [完整 LICENSE](licenses/upstream/lxserver-reference/LICENSE)、[README 适用协议节](licenses/upstream/lxserver-reference/README.protocol.md) |
| XCQ0607/lxserver，当前核对的 main | `5c220b978ac1864d0a367930e579f009dfe7f933` | [完整 LICENSE](licenses/upstream/lxserver/LICENSE)、[README 适用协议节](licenses/upstream/lxserver/README.protocol.md) |
| lyswhut/lx-music-desktop，当前核对的 master | `9c364b482e5621a1d38b50e8610d2fb974457e6e` | [完整 LICENSE](licenses/upstream/lx-music-desktop/LICENSE)、[README 适用协议节](licenses/upstream/lx-music-desktop/README.protocol.md) |

两个上游的 README **都明确称其协议是 Apache-2.0 的补充，并称冲突时以补充协议为准**。副本完整保留适用章节，包括数据来源、版权数据、免责声明、使用限制、非商业性质和接受协议等内容；不将其改写为本项目自行增加的一份许可，也不忽略其适用问题。商业使用、附加条款效力或适用范围存在疑问时，应向权利人或专业人士确认，不能只凭 Apache 标签作无限制商用保证。

lxserver LICENSE 末尾的 `Copyright 2018 File Browser Contributors` 及附随条款原样保留。README 中的 `Apache License 2.0 copyright (c) 2026 [xcq0607](https://github.com/xcq0607)` 也完整保留。对 lx-music-desktop 的作者与贡献者继续保留上游归属，不填造上游 LICENSE 模板中的版权年份。

87 个 vendor 源文件中有 65 个与参考来源树逐字节相同，22 个有已记录的修改；当前核对树的结果为 57 个相同、30 个不同。歌词工具目录在两棵树中的路径不同，已逐文件映射。**最初导入的上游提交未知**，这些比较结果不能证明最初导入版本。证据见 [逐文件摘要](licenses/vendor-provenance.json) 与 [修改记录](js-bridge/vendor/PATCHES.md)。本次仅补记修改标记，不改变 SDK 行为。

### SDK 内已注明的再上游来源

- `musicSdk/wy/utils/crypto.js`、`musicSdk/wy/songList.js` 原注释引用 `Binaryify/NeteaseCloudMusicApi`。其原 GitHub 文件在核对时返回 404；从同名 npm **4.32.0** 发布包获取并核对了完整性，保留 [MIT LICENSE](licenses/upstream/NeteaseCloudMusicApi/LICENSE)（`Copyright (c) 2013-2022 Binaryify`）。发布元数据报告 gitHead `4d63e5562199115915f2ee855460ef8999873b53`，但这不是已经证明的最初导入提交。包地址、完整性值与此限制记录于 [清单](licenses/manifest.json)。
- `musicSdk/kg/comment.js` 原注释引用 `GitHub-ZC/wp_MusicApi` 的固定提交 `bf9307dd138dc8ac6c4f7de29361209d4f5b665f`。已保留该提交的 [Apache-2.0 LICENSE](licenses/upstream/wp_MusicApi/LICENSE)，并保留代码中的原始引用。
- `musicSdk/kg/vendors/infSign.min.js` 与两个核对版本的 lxserver 文件一致，未找到其在该上游中另附的独立来源/许可说明。本次按上游文件及其声明归档，没有把该事实扩张为对更深层权属的独立保证。

## JS bundle 与 Go 二进制

[组件与版本索引](licenses/README.md)、[机器可读清单](licenses/manifest.json) 和 [源码内联声明](licenses/GO_SOURCE_NOTICES.txt) 一起构成随附材料。原文优先取自本地精确版本源码，不只列许可证名称或链接。

- JS bundle 实际输入由 esbuild 元数据生成到 `js-bridge/bundle-inputs.json`。当前包括 `buffer@6.0.3`、`base64-js@1.5.1`（MIT）和 `ieee754@1.2.1`（BSD-3-Clause）。另保守随附 `esbuild@0.28.2` 的 MIT 原文，涵盖生成辅助代码；Bun、Node 和 esbuild 工具程序本身不进入最终镜像。打包保留法律注释，并指向随附完整许可。
- Go 部分按 `go.mod` 精确版本及 `CGO_ENABLED=0`、`linux/amd64` / `linux/arm64` 的 `go list -deps ./cmd/lxsc` 并集收集，当前有 **21 个外部模块**。模块的相关 LICENSE、NOTICE、PATENTS、AUTHORS 及内联第三方声明一并保留；模块内未被链接的附带许可可能多于最终逐函数需求。
- `go.mod` 中的 `github.com/mattn/go-isatty`、`github.com/ncruces/go-strftime` 当前不在上述两个目标的运行包并集中；纯测试/构建工具模块也不据此声称进入应用二进制。新增目标、构建标签或升级版本必须重新核对。
- Go **1.27.0** 标准库保留 BSD 原文、实际使用的 `src/vendor/golang.org/x/*` 许可及 PATENTS，以及 Plan 9/Lucent、Sun、Cephes、fiat-crypto、内部 profile 等源码中的其他声明。
- goja 的 MIT 不能覆盖其 V8/Lucent 浮点转换代码，已另存原文；`age` 的 bech32 有独立 MIT 声明；`yaml.v3` 有 libyaml 的 MIT、其余文件的 Apache-2.0 和独立 NOTICE；`golang-lru` 含 MPL-2.0 与链表 BSD 声明。
- `modernc.org/sqlite@v1.58.0` 包含 SQLite **3.53.4**。已保留驱动 BSD、SQLite 公共领域声明及 sqlite-vec 的 MIT 原文；不是将 SQLite 一概写成驱动的 BSD 或本项目的 Apache。`modernc.org/memory` 的 Go/mmap-go 声明、`modernc.org/libc` 的 musl、go-netdb、Nixpkgs、uint128、UUID 及其他内联声明也分别保留。

### MPL 与 LGPL 涉及的源码、修改和重新编译

`github.com/hashicorp/golang-lru/v2@v2.0.7` 的 MPL-2.0 覆盖文件仍按 MPL 提供，本项目没有修改这些依赖文件。完整对应模块源码可从 [Go 模块代理 ZIP](https://proxy.golang.org/github.com/hashicorp/golang-lru/v2/@v/v2.0.7.zip) 或 [上游 v2.0.7](https://github.com/hashicorp/golang-lru/tree/v2.0.7) 取得；本目录附带完整 MPL 文本及原版权声明。

`modernc.org/libc@v1.75.6` 的以下实际依赖翻译单元含 GNU C Library 的 **LGPL-2.1-or-later** 头文件声明及 UUID 的 BSD 等声明，不能把整个模块仅标为 BSD：

- `sys/types/types_linux_amd64.go`、`sys/types/types_linux_arm64.go`
- `uuid/uuid/uuid_linux_amd64.go`、`uuid/uuid/uuid_linux_arm64.go`

这四个文件已**完整原样**归档到 [headers/](licenses/go/modernc.org/libc@v1.75.6/headers/)，以 `.go.txt` 保存，不参与 lxsc 编译，并在清单中记录 SHA-256；另附 [LGPL-2.1 全文](licenses/texts/LGPL-2.1.txt)。核对版本中这四个单元只有头文件声明、常量、布局和生成器占位变量，没有 Go 函数实现。LGPL 第 5 节包含头文件数值参数/数据布局等情形的规则，但本项目不声称已经对该例外的适用作出法律定论。完整对应模块源码可从 [精确版本 ZIP](https://proxy.golang.org/modernc.org/libc/@v/v1.75.6.zip) 获取；随附源码、声明和许可不能由这一边界说明替代。

获取对应源码并自行修改/替换后重编译的基本方法如下（示例路径由使用者选择）：

```bash
# 在完整 lxsc 源码目录执行；输出中的 Dir/Zip 是该版本的源码位置。
go mod download -json github.com/hashicorp/golang-lru/v2@v2.0.7 modernc.org/libc@v1.75.6
# 将需要修改的模块源码复制到可写目录，保留其许可与声明，然后指向该副本：
go mod edit -replace=modernc.org/libc=/absolute/path/to/modified-libc
# 如需替换 MPL 模块，采用相同方式：
go mod edit -replace=github.com/hashicorp/golang-lru/v2=/absolute/path/to/modified-golang-lru
CGO_ENABLED=0 go build -trimpath -o lxsc ./cmd/lxsc
```

只替换需要修改的模块即可。跨架构可再设置 `GOOS=linux GOARCH=arm64`。本项目不附加禁止替换依赖、重新链接或为调试这些修改进行逆向工程的限制；再分发修改版时仍应履行相应原许可。许可校验器对 `replace` 或版本漂移会有意报错，需为新的发行版重新审核和更新清单，而不是跳过应履行的义务。

## Alpine 基础镜像

Docker 运行层使用 `alpine:3.24`，另安装 `ca-certificates` 和 `tzdata`。根据 [Alpine 官方支持周期](https://alpinelinux.org/releases/)（2026-09-06 核对），原 3.20 已于 2026-04-01 结束常规支持，3.24 的 main 软件源支持至 2028-06-01；本次只更新待发布镜像，不自动改变已有部署。这些系统组件不是 lxsc 原创代码，也不统一适用本项目 Apache 授权。每个实际镜像将 `/lib/apk/db/installed` 的原始包记录完整复制到 `/usr/share/licenses/lxsc/ALPINE_PACKAGES.txt`；其中 `P:` / `V:` / `L:` / `o:` / `c:` 分别是包名、版本、许可证、源码包名与 aports 配方提交。

[alpine/](licenses/alpine/) 补充保存 GPL-2.0 全文，以及首次核对的 OpenSSL 3.5.7、musl 1.2.6、zlib 1.3.2 的许可/版权原文；MPL-2.0 全文另见 [golang-lru 的许可副本](licenses/go/github.com/hashicorp/golang-lru/v2@v2.0.7/LICENSE)。这些副本不改变系统组件各自的许可，基础包升级后应依据实际包记录重新检查。

Alpine 的源码、包配方及各包许可须按实际版本另行查阅：[Alpine 包数据库](https://pkgs.alpinelinux.org/packages?branch=v3.24)、[v3.24 软件源](https://dl-cdn.alpinelinux.org/alpine/v3.24/)、[源码镜像](https://distfiles.alpinelinux.org/distfiles/)、[aports](https://gitlab.alpinelinux.org/alpine/aports)。使用包记录中的 `c:` 提交及 `o:` 源码包名，可在 `https://github.com/alpinelinux/aports/tree/<提交>/main/<源码包名>` 定位对应配方、补丁及源码下载与校验信息。本目录的应用许可归档不声称替代对 BusyBox、musl、证书、时区数据等基础系统组件的独立义务；再次分发完整镜像时也须保留和满足这些组件的许可及源码提供要求。

## 更新与检查

先用固定 Bun 和锁文件重建，再执行：

```bash
(cd js-bridge && bun install --frozen-lockfile && bun run build)
node scripts/licenses.mjs --check
```

校验包括精确依赖集合、版本、原文摘要、实际源码副本、双架构内联声明以及 vendor 修改标记。审核过变更、手工更新组件清单后，可执行 `node scripts/licenses.mjs --update` 从已安装的精确版本源码重新复制并生成摘要；它不会联网猜测授权，也不会自动放行未列出的依赖或替换模块。静态上游归档需按固定提交另行核对后更新，不能用运行 `--update` 来自动改写其法律文本。
