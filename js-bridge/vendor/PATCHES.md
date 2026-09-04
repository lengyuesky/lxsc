# vendor 目录相对上游（XCQ0607/lxserver `src/modules/utils`）的修改

- `request.js`：移除 needle/tunnel/代理逻辑，代理由 Go 宿主处理；其余保持一致。
- `musicSdk/api-source.js`：改为存根，musicUrl 由宿主通过音源脚本获取。
- `index.js`：追加 `toMD5`（bd 模块引用）。
- `musicSdk/kw/lyric.js`：`RegExp.$1` → `result[1]`（goja 不支持 RegExp 静态属性）。
- `musicSdk/mg/musicInfo.js`：`RegExp.$1` → 使用 `exec` 结果；修复 `Promise.all(Promise)` 导致的"object is not iterable"。
- 删除各平台 `api-test.js`、`temp/`、`kw/api-temp.js`（未使用）。
- `musicSdk/mg/lyric.js`：resourceinfo 返回空时回退使用 songInfo 自带的 lrcUrl/mrcUrl。
