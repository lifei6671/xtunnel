// Package webui 暴露由 Vite 生成并嵌入 Server 的管理控制台资源。
package webui

import "embed"

// Dist 保存 web/dist 下的生产构建产物。
//
//go:embed all:dist
var Dist embed.FS
