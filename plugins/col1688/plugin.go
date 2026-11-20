package col1688

import (
	"github.com/wailsapp/wails/v3/pkg/application"
	"ltools/internal/plugins"
)

// Plugin 1688 采集插件实现。
type Plugin struct {
	*plugins.BasePlugin
	app     *application.App
	dataDir string
	service *Collect1688Service
}

// NewPlugin 创建 1688 采集插件。
func NewPlugin() *Plugin {
	p := &Plugin{
		BasePlugin: plugins.NewBasePlugin(&plugins.PluginMetadata{
			ID:          "c1688.builtin",
			Name:        "1688 工厂供应商采集",
			Version:     "1.0.0",
			Author:      "LTools",
			Description: "采集 1688 工厂/供应商的名称、电话、地址，支持浏览器自动采集与粘贴 HTML 解析，结果可导出 CSV。",
			Type:        plugins.PluginTypeBuiltIn,
			// 显式声明为 installed：Manager.Register 会把 installed 视为默认启用并启动，
			// 若不设置，State 为空会导致该插件被判定为禁用，前端 Home（仅展示 enabled）就看不到它。
			State:       plugins.PluginStateInstalled,
			Permissions: []plugins.Permission{plugins.PermissionNetwork, plugins.PermissionFileSystem},
			Keywords:    []string{"1688", "采集", "工厂", "供应商", "电话", "地址", "1688采集"},
		}),
	}
	return p
}

// Init 初始化插件（记录依赖）。
func (p *Plugin) Init(app *application.App) error {
	p.app = app
	return nil
}

// SetDataDir 设置数据目录（与 hosts/tunnel 等插件一致的可选注入）。
// 同时惰性创建唯一的 Collect1688Service 实例（后续 Service() 返回它）。
func (p *Plugin) SetDataDir(dir string) error {
	p.dataDir = dir
	if p.service == nil {
		p.service = NewCollect1688Service(appOf(p), dir)
	}
	return nil
}

// NewService 创建并返回可暴露给前端的采集服务（单例：已存在则复用，避免多实例
// 各自 registerEvents 导致多个 Event.On 监听器 + 各自独立 channel 的竞争）。
func (p *Plugin) NewService(app *application.App, dataDir string) *Collect1688Service {
	p.app = app
	p.dataDir = dataDir
	if p.service == nil {
		p.service = NewCollect1688Service(app, dataDir)
	}
	return p.service
}

// Service 返回插件持有的唯一采集服务实例（供 main.go 注册到 Wails，避免再 new 第三个）。
// 若尚未创建（SetDataDir/NewService 都未跑过），惰性创建。
func (p *Plugin) Service() *Collect1688Service {
	if p.service == nil {
		p.service = NewCollect1688Service(appOf(p), p.dataDir)
	}
	return p.service
}

// appOf 返回插件绑定的 app（SetDataDir 在 Init 前调用时的兜底）。
func appOf(p *Plugin) *application.App {
	return p.app
}
