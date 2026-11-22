//go:build windows
// +build windows

// Package edge 暴露 WebView2 的额外初始化脚本机制。
//
// 应用层（如 col1688 插件）可通过 SetExtraInitScripts 在 WebView2 初始化完成后
// 注入额外的 JS（跨导航，每个新文档都执行），不经过 ExecJS COM 调用（不碰 bad state）。
//
// 每个脚本独立 AddScriptToExecuteOnDocumentCreated 注册（规避单脚本超长被 COM 静默丢弃）。
//
// 这是 Wails 框架的 patch 层（third_party/wails），升级 Wails 时需重打补丁。
package edge

import (
	"github.com/wailsapp/wails/v3/internal/webview2/pkg/edge"
)

// SetExtraInitScripts 设置多个额外初始化脚本（委托给 edge.ExtraInitScripts）。
// 每个脚本独立通过 AddScriptToExecuteOnDocumentCreated 注入（跨导航，每个新文档执行）。
// 用于把大脚本（bridgeJS / suppliersJS）拆开，避免单脚本超长被 WebView2 COM 静默丢弃。
func SetExtraInitScripts(scripts ...string) {
	edge.SetExtraInitScripts(scripts...)
}

// GetExtraInitScripts 获取当前额外初始化脚本列表。
func GetExtraInitScripts() []string {
	return edge.ExtraInitScripts
}
