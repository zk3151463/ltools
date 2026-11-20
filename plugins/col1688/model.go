// Package col1688 实现 1688 工厂/供应商信息（名称、电话、地址）采集插件。
//
// 设计参考 dryygjx（Wails + go-rod）的后台采集界面：
// 参数区 + 控制条 + 运行日志 + 结果表（名称/电话/地址 + 导出 CSV）。
//
// 采集有两种数据源：
//  1. browser  模式：go-rod 驱动真实 Chrome，登录后驱动 1688 搜索/翻页（贴近参考架构）。
//  2. html     模式：粘贴 1688 页面 HTML，本地解析（始终可用、可离线验证）。
package col1688

import "time"

// Supplier 一条采集到的供应商/工厂记录。
type Supplier struct {
	Name    string `json:"name"`    // 名称（公司名/工厂名）
	Phone   string `json:"phone"`   // 电话
	Address string `json:"address"` // 地址（省市 + 详细地址）
	Main    string `json:"main"`    // 主营业务（可选）
	Level   string `json:"level"`   // 实力等级（如 实力商家/诚信通年限）
	URL     string `json:"url"`     // 来源链接
	Source  string `json:"source"`  // 来源描述（如 "搜索:帽子 第2页"）
	Time    string `json:"time"`    // 采集时间（RFC3339 本地格式）
}

// 去重键：名称 + 地址
func (s *Supplier) dedupKey() string {
	return s.Name + "||" + s.Address
}

// CollectOptions 一次采集的参数。
type CollectOptions struct {
	// Mode 数据源模式：browser | html
	Mode string
	// Keyword 关键词（browser 模式必填）
	Keyword string
	// Category 类别：factory(找工厂) | supplier(找供应商) | offer(找货源)
	Category string
	// Region 地区限定（如 浙江、广东），可选
	Region string
	// MaxCount 最大采集条数（0 表示不限）
	MaxCount int
	// Pages browser 模式最多翻几页（0 表示 1 页）
	Pages int
	// DelaySec 翻页/访问间隔秒数（防风控）
	DelaySec int
	// HTML html 模式下粘贴的页面内容
	HTML string
}

// CollectResult 采集结果。
type CollectResult struct {
	Items  []Supplier `json:"items"`
	Total  int        `json:"total"`
	Done   bool       `json:"done"`
	Message string     `json:"message"`
}

// RunState 采集运行状态机。
type RunState int

const (
	StateIdle RunState = iota
	StateRunning
	StatePaused
	StateStopped
)

// LogEntry 一条运行日志。
type LogEntry struct {
	Time string `json:"time"`
	Msg  string `json:"msg"`
}

// nowLocal 统一的本地时间戳格式。
func nowLocal() string {
	return time.Now().Format("2006-01-02 15:04:05")
}
