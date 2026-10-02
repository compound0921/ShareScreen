// Package webui 内嵌控制页的静态资源。
//
// 全部资源打进二进制,不依赖 CDN —— 控制页是本机页面,
// 断网时也必须能打开,而且要能在无外网的局域网环境里工作。
package webui

import "embed"

//go:embed static
var Files embed.FS
