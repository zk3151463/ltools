//go:build windows
// +build windows

// Package edge 暴露 WebView2 的额外初始化脚本机制。
//
// 应用层（如 col1688 插件）可通过设置 ExtraInitScript 在 WebView2 初始化完成后
// 注入额外的 JS（跨导航，每个新文档都执行），不经过 ExecJS COM 调用。
//
// 这是 Wails 框架的 patch 层（third_party/wails），升级 Wails 时需重打补丁。
package edge

import (
	"github.com/wailsapp/wails/v3/internal/webview2/pkg/edge"
)

// ExtraInitScript 是应用层可设置的额外初始化脚本。
// 在 WebView2 初始化完成后，与 window.external.invoke 一起通过
// AddScriptToExecuteOnDocumentCreated 注入（跨导航，每个新文档都执行）。
// col1688 插件用它注入 bridgeJS + suppliersJS（不经过 ExecJS COM 调用，不碰 bad state）。
var ExtraInitScript string

// SetExtraInitScript 设置额外初始化脚本（委托给 edge.ExtraInitScript）。
func SetExtraInitScript(script string) {
	edge.ExtraInitScript = script
}

// GetExtraInitScript 获取当前额外初始化脚本。
func GetExtraInitScript() string {
	return edge.ExtraInitScript
}
