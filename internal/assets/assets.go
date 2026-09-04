// Package assets 内嵌 JS 桥接产物与统一控制台静态文件
package assets

import "embed"

//go:embed js/prelude.js js/sdk.bundle.js
var JS embed.FS

//go:embed web
var Web embed.FS
