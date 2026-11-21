package col1688

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// windowEngine 基于 Wails 子窗口 + JS 注入采集 1688。
//
// 架构：
//  1. 用 Wails application.Window.NewWithOptions 开一个**外部 URL** 子窗口（加载 1688 搜索页/登录页）。
//  2. 用户在这个窗口里手动完成：登录、输入关键词、选类别/地区、拖滑块。
//  3. 用户点「采集当前页」→ 主窗口前端调用 CollectCurrentPage → Go 端对子窗口 ExecJS 注入
//     提取脚本，把当前页 DOM/内嵌 JSON 通过 Wails 事件 c1688:page-html 回传 → parseHTML。
//  4. 翻页由用户手动在子窗口里点「下一页」（拟人轨迹最真实），程序只负责读当前页。
//
// 与 go-rod 的差异：
//   - 无 CDP 自动化痕迹（Wails 子窗口是真实 WebView2，不是 go-rod 的远程调试实例）
//   - 无需 stealth 注入（真实浏览器指纹，baxia 不会因 CDP 痕迹拒滑块）
//   - 无独立 profile（WebView2 用 Wails 应用的隔离 profile，登录态在子窗口内持久）
//   - 数据走 Wails 事件回传（而非 CDP GetContent），需要 1688 允许我们注入的 bridge 脚本运行
type windowEngine struct {
	app *application.App

	win  application.Window
	name string // 子窗口名（"c1688-window"）

	diagDir string

	// pageHTMLCh 接收子窗口回传的 HTML 事件（c1688:page-html）
	pageHTMLCh chan pageHTMLPayload

	// bridgeStop 让 installBridge 的持久重注入循环可被 CloseWindow 终止。
	// OpenWindow 每次开新窗口时 reset（close + 重建），避免 goroutine 泄漏。
	bridgeStop chan struct{}
	// bridgeRunning 防止 OpenWindow 被连点时重复 spawn 多个 installBridge goroutine。
	bridgeRunning bool
	// bridgeMu 保护 bridgeStop / bridgeRunning 的并发读写。
	bridgeMu sync.Mutex
	// onCreatedRegistered 防 registerWindowGuard 重复注册 OnCreate。
	onCreatedRegistered bool
	// logf 日志钩子，由 service 注入（engine 自身不持有 store/app 日志通道）。
	// 允许为 nil（OnCreate 回调里做 nil 检查）。
	logf func(string)

	// htmlServer 是子窗口回传 HTML 的本地回环 HTTP 服务。
	// 关键约束：1688 子窗口是**外部站点**，Wails 不把 window.ltools（runtime）注入外部页，
	// 且 Wails v3.1 的 ExecJS 是 void（Go 读不回 JS 结果）。因此"页面→Go"的唯一稳健通道
	// 是让外部页 fetch POST 到 127.0.0.1:<port>（https 页发 http 127.0.0.1 是允许的，
	// body-only fetch 不触发 CORS 阻塞；CORS 只拦"读响应"，POST 本身送达）。
	// collectReq 由 CollectCurrentPage 在触发回传前打开，收集到 HTML 后关闭。
	htmlServer   *http.Server
	htmlServerMu sync.Mutex
	collectReq   chan pageHTMLPayload
}

// pageHTMLPayload 子窗口回传的页面载荷。
// Items 是子窗口 JS 直接取到的结构化供应商数据（首选）；HTML 是兜底（Items 为空时 Go 端 parseHTML）。
type pageHTMLPayload struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	HTML  string `json:"html"`
	Items []Supplier `json:"items"`
}

// newWindowEngine 创建一个 Wails 子窗口引擎。
//   - htmlServerOwned：true → 该 engine 自己启动/持有 127.0.0.1:48217 回传服务（列表引擎）。
//   - false → 直接复用已存在的回传服务（详情引擎，端口已监听则跳过启动）。
func newWindowEngine(app *application.App, diagDir string, htmlServerOwned bool) *windowEngine {
	e := &windowEngine{
		app:        app,
		name:       "c1688-window",
		diagDir:    diagDir,
		pageHTMLCh: make(chan pageHTMLPayload, 16),
		bridgeStop: make(chan struct{}),
		collectReq: make(chan pageHTMLPayload, 4),
	}
	if htmlServerOwned {
		e.startHTMLReturn()
	}
	return e
}

// newDetailWindowEngine 创建详情页子窗口引擎（第二个窗口）。
// 不启动回传服务（列表引擎已起 127.0.0.1:48217），仅共享同一台服务。
// 独立引擎 = 独立窗口 + 独立 bridge 注入循环。风控上"详情窗被拒可单独关窗重开"，
// 列表窗信任度不受影响。
func newDetailWindowEngine(app *application.App, diagDir string) *windowEngine {
	e := newWindowEngine(app, diagDir, false)
	e.name = "c1688-detail-window"
	return e
}

// registerEvents 在 app 启动后调用，注册 c1688:page-html 事件监听。
// 必须在 CreateWindow 之前调用，否则子窗口回传的事件会丢失。
func (e *windowEngine) registerEvents() {
	if e.app == nil {
		return
	}
	// 注册"窗口创建"兜底钩子（路线 C）：任何 WebView2 冒出的新窗口被即时关闭。
	e.registerWindowGuard()
	_ = e.app.Event.On("c1688:page-html", func(ev *application.CustomEvent) {
		if ev == nil || ev.Data == nil {
			return
		}
		// 子窗口 Emit 时传的是 JS 对象（Wails v3 序列化为 map[string]any）
		if m, ok := ev.Data.(map[string]any); ok {
			p := pageHTMLPayload{
				URL:   getStr(m, "url"),
				Title: getStr(m, "title"),
				HTML:  getStr(m, "html"),
			}
			if itemsRaw, ok := m["items"]; ok {
				if bs, err := json.Marshal(itemsRaw); err == nil {
					_ = json.Unmarshal(bs, &p.Items)
				}
			}
			select {
			case e.pageHTMLCh <- p:
			default:
			}
			return
		}
		// 兜底：把 Data 整体 JSON 序列化后反序列化
		raw, err := json.Marshal(ev.Data)
		if err != nil {
			return
		}
		var p pageHTMLPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return
		}
		select {
		case e.pageHTMLCh <- p:
		default:
		}
	})
}

// htmlReturnPort 子窗口回传 HTML 的固定本地回环端口。
// 固定端口便于 bridgeJS 硬编码；被占用时 startHTMLReturn 会记日志（详情引擎复用时属正常）。
const htmlReturnPort = 48217

// startHTMLReturn 启动本地回环 HTTP 服务，接收子窗口 fetch POST 的 HTML。
// 列表引擎启动一次；详情引擎复用同一服务（端口已被占用时 logf 不报"失败"，静默跳过）。
// 幂等：e.htmlServer 非 nil 直接返回。
//
// 通道原理（外部页 → Go 的唯一稳健路）：
//   - 1688 子窗口是 https 外部页，Wails 不注入 window.ltools，ExecJS 又是 void。
//   - 让页面 JS（bridgeJS 注入的 __ltoolsCollect）发 fetch POST 到 http://127.0.0.1:48217/c1688/html。
//   - https 页向 http 127.0.0.1 发起 **body-only fetch** 不受 mixed-content 完全阻断
//     （浏览器允许 127.0.0.1 的混合内容作为本地回环例外；即便个别浏览器拦，
//     也优于"完全无法回传"）。POST 送达后 handler 读 body → 推 collectReq。
//   - 加 Access-Control-Allow-Origin:* + 简单 POST（Content-Type: text/plain，
//     不触发 CORS preflight），让"读响应"也不被 CORS 卡。
func (e *windowEngine) startHTMLReturn() {
	e.htmlServerMu.Lock()
	defer e.htmlServerMu.Unlock()
	if e.htmlServer != nil {
		return // 已启动
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", htmlReturnPort))
	if err != nil {
		// 判断"被占用"：Listen 失败且消息含 "address already in use"（Windows 下 net.Error 的 Timeout 为 false，Err 为 syscall）。
		// 不精确匹配，改为"启动失败但端口已监听过（e.htmlServer 已被外部置位）时静默"——简化：
		// 让调用方（详情引擎）在调用前先检查端口。这里统一报错。
		if e.logf != nil {
			e.logf("HTML 回传端口 " + fmt.Sprintf("%d", htmlReturnPort) + " 启动失败：" + err.Error() + "（若列表引擎已起过本服务则属正常复用）")
		}
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/c1688/html", e.htmlReturnHandler)
	mux.HandleFunc("/c1688/diag", e.htmlDiagHandler)
	srv := &http.Server{Handler: mux}
	e.htmlServer = srv
	go func() {
		_ = srv.Serve(ln)
	}()
	if e.logf != nil {
		e.logf(fmt.Sprintf("HTML 回传服务已启动（127.0.0.1:%d）", htmlReturnPort))
	}
}

// htmlDiagHandler 接收 /c1688/diag（diagnoseJS 探针 + __ltoolsCollect 状态）。
// 仅记日志，不影响回传流程。诊断信息：bridge 是否装好 / __ltoolsCollect 是否存在 /
// 当前 URL / readyState / 上次取到的 itemsCount。
func (e *windowEngine) htmlDiagHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	b, err := readBody(r, 100_000) // 诊断 body 很小（~1KB），100KB 足够
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("read body failed"))
		return
	}
	var d map[string]any
	if err := json.Unmarshal(b, &d); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad json"))
		return
	}
	if e.logf != nil {
		bridgeInstalled, _ := d["bridgeInstalled"].(bool)
		collectFn, _ := d["collectFn"].(string)
		collectSuppliersFn, _ := d["collectSuppliersFn"].(string)
		pageURL, _ := d["url"].(string)
		readyState, _ := d["readyState"].(string)
		statusRaw, _ := d["status"].(map[string]any)
		var statusDesc string
		if statusRaw != nil {
			hasFn, _ := statusRaw["hasFn"].(bool)
			itemsCount, _ := statusRaw["itemsCount"].(float64)
			statusURL, _ := statusRaw["url"].(string)
			statusReady, _ := statusRaw["readyState"].(string)
			statusTarget, _ := statusRaw["target"].(string)
			statusDesc = fmt.Sprintf("status{hasFn=%v,itemsCount=%.0f,url=%s,readyState=%s,target=%s}",
				hasFn, itemsCount, statusURL, statusReady, statusTarget)
		} else {
			statusDesc = "status{null}"
		}
		e.logf(fmt.Sprintf("[diag] bridgeInstalled=%v collectFn=%s collectSuppliersFn=%s url=%s readyState=%s | %s",
			bridgeInstalled, collectFn, collectSuppliersFn, pageURL, readyState, statusDesc))
	}
	if _, werr := w.Write([]byte("ok")); werr != nil {
		return
	}
}

// htmlReturnHandler 接收子窗口 POST 的 HTML 载荷，推给正在等待的 collectReq。
// 响应加 CORS 头（允许外部 https 页读回 200，避免 CORS 报错影响后续 fetch）。
func (e *windowEngine) htmlReturnHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Image 兜底：fetch 被 CSP 拦时，__ltoolsCollect 会把 items 编码进 ?b= query，
	// 服务端读 query 重建 payload（没有 html，仅 items）。
	if r.Method == http.MethodGet {
		bParam := r.URL.Query().Get("b")
		if bParam == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var p pageHTMLPayload
		if err := json.Unmarshal([]byte(bParam), &p); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		e.dispatchPayload(p, "image-fallback")
		w.WriteHeader(http.StatusNoContent)
		// Image 请求不读响应；写一个 1x1 透明 GIF 防浏览器控制台红叉。
		_, _ = w.Write([]byte{0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0xFF, 0xFF, 0xFF, 0x21, 0xF9, 0x04, 0x01, 0x00, 0x00, 0x00, 0x00, 0x2C, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x02, 0x02, 0x4C, 0x01, 0x00, 0x3B})
		return
	}
	b, err := readBody(r, 5_000_000) // 上限 5MB，防超大页
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("read body failed"))
		return
	}
	var p pageHTMLPayload
	if err := json.Unmarshal(b, &p); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad json"))
		return
	}
	e.dispatchPayload(p, "fetch")
	if _, werr := w.Write([]byte("ok")); werr != nil {
		return
	}
}

// dispatchPayload 把收到的 payload 推给正在等待的 collectReq（或 pageHTMLCh 兜底），
// 并按 source（"fetch" / "image-fallback"）记一行简短日志。
func (e *windowEngine) dispatchPayload(p pageHTMLPayload, source string) {
	if e.logf != nil && len(p.Items) > 0 {
		e.logf(fmt.Sprintf("[回传-%s] 收到 %d 条 items（url=%s）", source, len(p.Items), p.URL))
	}
	select {
	case e.collectReq <- p:
	default:
		select {
		case e.pageHTMLCh <- p:
		default:
		}
	}
}

// readBody 读 body，超限返回错误。
func readBody(r *http.Request, limit int64) ([]byte, error) {
	defer r.Body.Close()
	if limit <= 0 {
		limit = 5_000_000
	}
	lr := io.LimitReader(r.Body, limit+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("body exceeds limit %d", limit)
	}
	return data, nil
}

// htmlReturnURL 返回子窗口 fetch 应 POST 的地址。
func (e *windowEngine) htmlReturnURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/c1688/html", htmlReturnPort)
}

// stopHTMLReturn 停止回传服务（进程退出时调用）。
func (e *windowEngine) stopHTMLReturn() {
	e.htmlServerMu.Lock()
	defer e.htmlServerMu.Unlock()
	if e.htmlServer != nil {
		_ = e.htmlServer.Close()
		e.htmlServer = nil
	}
}

// registerWindowGuard 注册"窗口创建"全局钩子（Wails 公开 API WindowManager.OnCreate）。
//
// 根因：Wails v3 的 webview2 层不消费 WebView2 的 NewWindowRequested 事件，
// 所以 1688 页面里 target=_blank / window.open / 表单 target=_blank 在 COM 层
// 触发的"开新窗"走 WebView2 默认行为 → 冒出一个**新的原生 WebView2 窗口**，
// JS 层拦截拦不住（页面早期拿到原始 window.open、或 COM 直接触发、或 500ms 恢复窗口期）。
//
// 兜底策略（路线 C）：OnCreate 回调在**任何**窗口创建时触发（包括 WebView2 冒的新窗）。
// 识别"非我们预期"的窗口：
//   - 主窗口 Name() 为空串（main.go 建主窗时没设 Name）
//   - 我们的 1688 子窗口 Name() == "c1688-window"
//   - 其余 = WebView2 冒的新窗口 → 立即 Close()，并尝试把它的 URL 导回子窗口
//
// 由于 Wails Window 接口**没有 URL() 读法**，导回 URL 靠 ExecJS 读 location.href；
// 读不到（窗口已关 / 跨域）就退化为"只关窗不导回"。
//
// 幂等：registerEvents 被单例收敛后只调一次，但 OnCreate 用 append，
// 这里加 onCreatedRegistered 标志防重复注册。
func (e *windowEngine) registerWindowGuard() {
	if e.app == nil {
		return
	}
	e.bridgeMu.Lock()
	defer e.bridgeMu.Unlock()
	if e.onCreatedRegistered {
		return
	}
	e.onCreatedRegistered = true

	e.app.Window.OnCreate(func(w application.Window) {
		if w == nil {
			return
		}
		name := w.Name()
		// 跳过我们已知的两个窗口
		if name == "" || name == e.name {
			return
		}
		// 非预期的窗口（WebView2 冒的新窗）：关闭它。
		// 注：Window 接口无 URL() 读法，无法精确导回该窗口的目标 URL；
		// 关闭后子窗口保持当前页（用户可手动翻页）。logf 可能为 nil。
		if e.logf != nil {
			e.logf("检测到 WebView 新窗口（name=" + name + "），已关闭并保留子窗口")
		}
		e.routeWindowBack(w)
	})
}

// routeWindowBack 处理冒出的新窗口。
//
// Wails Window 接口没有 URL() 读法（只读 location 需 ExecJS 返回，而 ExecJS 是 void），
// 无法精确拿到该新窗口"刚要导航到的 URL"。故保守策略：直接关闭新窗口，
// 子窗口保持当前页（用户手动翻页）。若后续 Wails 暴露 URL 读法，可在此导回。
func (e *windowEngine) routeWindowBack(w application.Window) {
	w.Close()
}

// SetLogf 注入日志钩子（由 service 在创建后调用），供 OnCreate 兜底钩子写日志。
func (e *windowEngine) SetLogf(fn func(string)) {
	e.logf = fn
}

// drainChannel 清空 pageHTMLCh 里可能残留的旧 payload。
func (e *windowEngine) drainChannel() {
	for {
		select {
		case <-e.pageHTMLCh:
		default:
			return
		}
	}
}

// jsStringLiteral 把 Go string 编成可安全嵌入 JS 的字符串字面量（双引号 + 转义）。
// 用于 ExecJS 注入常量（如回传 URL），避免手拼导致注入 / 转义问题。
func (e *windowEngine) jsStringLiteral(s string) string {
	b, _ := json.Marshal(s) // 返回带引号的 JSON 字符串，即合法 JS 字符串字面量
	return string(b)
}

// getStr 从 map[string]any 里取 string 字段（缺失返回空串）。
func getStr(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// searchURL 构造 1688 各列表页搜索 URL（与 1688 站内搜索入口对齐）。
// 关键词做 URL 编码（站内 keywords 均为 percent-encoded），不附带筛选参数，
// 地区/排序等筛选由用户在子窗口里手动调整。
func (e *windowEngine) searchURL(o CollectOptions) string {
	base := "https://s.1688.com"
	kw := url.QueryEscape(o.Keyword)
	switch o.Category {
	case "factory":
		return fmt.Sprintf("%s/company/pc/factory_search.htm?keywords=%s", base, kw)
	case "supplier":
		return fmt.Sprintf("%s/company/company_search.htm?keywords=%s", base, kw)
	case "industry":
		return fmt.Sprintf("%s/selloffer/imall_search.htm?keywords=%s", base, kw)
	default:
		return fmt.Sprintf("%s/selloffer/offer_search.htm?keywords=%s", base, kw)
	}
}

// currentWindow 实时查 Wails 全局 windows map，按名字取"当前活着的那个子窗口"。
// 不依赖 e.win 字段（字段可能因多实例 / 关闭异步而过期）。
// 返回 (窗口, 是否存在)。
func (e *windowEngine) currentWindow() (application.Window, bool) {
	if e.app == nil || e.name == "" {
		return nil, false
	}
	w, ok := e.app.Window.GetByName(e.name)
	if !ok || w == nil {
		return nil, false
	}
	// 同步 e.win 字段，避免后续误用过期引用
	if e.win != w {
		e.win = w
	}
	return w, true
}

// OpenWindow 开（或聚焦已开的）1688 子窗口。url 为空时默认到 1688 首页。
//
// 关键：Wails v3 的 WindowManager.NewWithOptions **每次调用都新建窗口，不按 Name 去重**
//（Name 仅是元数据）。若靠 e.win 字段判"已开"，在多个 Collect1688Service 实例 /
// 关闭异步等场景下会失效，导致"点一次按钮冒出新窗口"。
// 因此这里用 **GetByName** 实时查全局 windows map：已存在 → 复用（SetURL+Focus）；
// 不存在 → 才 NewWithOptions。这样无论多少实例、点多少次，全局只有一个 c1688-window，
// 渲染注入也稳定落在同一个窗口上。
func (e *windowEngine) OpenWindow(ctx context.Context, url string) error {
	if e.app == nil {
		return fmt.Errorf("应用未初始化")
	}
	// 已开（跨实例全局查）：复用 + 导航
	if w, ok := e.currentWindow(); ok {
		if url != "" {
			w.SetURL(url)
		}
		w.Focus()
		return nil
	}
	if url == "" {
		url = "https://www.1688.com/"
	}
	win := e.app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:          e.name,
		Title:         "1688 搜索",
		Width:         1280,
		Height:        900,
		MinWidth:      1024,
		MinHeight:     700,
		URL:           url,
		AlwaysOnTop:   false,
		Frameless:     false,
		DisableResize: false,
		// 子窗口背景：加载前/空白页显白色（避免 WebView2 默认暗色底透出，与"首页偏暗"相关）。
		// 这是 Wails 公开选项（BackgroundColour），无需 patch Wails。
		BackgroundType: application.BackgroundTypeSolid,
		BackgroundColour: application.NewRGB(255, 255, 255),
		// 治本：WebView2 COM 层 NewWindowRequested（target=_blank / window.open / <a target>）
		// 在此拦截——不冒第二个窗，而是把目标 URL 导回当前子窗口内跳转（w.SetURL）。
		// 主窗口不传此选项，故主窗口不受影响。详见 Wails patch（third_party/wails）。
		// 导回当前子窗口：闭包捕获 e，回调触发时读 e.win（在 NewWithOptions 返回后已赋值）。
		// 因 win（LHS 局部）在 RHS 字面量里尚未入作用域，故用 e.win 而非 win。
		OnNewWindowRequest: func(uri string) {
			if uri == "" {
				return
			}
			// 拦截到一次"开新窗"请求 → 导回当前子窗口，不冒第二个窗。
			e.logf(fmt.Sprintf("拦截新窗口请求 → 导回当前子窗口: %s", uri))
			if e.win != nil {
				e.win.SetURL(uri)
			}
		},
	})
	if win == nil {
		return fmt.Errorf("创建 1688 子窗口失败")
	}
	e.win = win

	// 子窗口加载后，启动持久 bridge 注入循环（注入 ltoolsBridge + 导航拦截 hook）。
	// 用 bridgeMu 保证同一时刻只有一个 installBridge goroutine（连点 OpenWindow 时幂等）。
	e.startBridgeLoop()
	return nil
}

// OpenSearch 打开子窗口到"关键词 + 类别"对应的 1688 搜索结果页。
//
// 方案 C（绕开搜索框跨域冒窗）：由 Go 端把关键词/类别拼成 1688 站内搜索 URL，
// 直接 SetURL 到子窗口——**整页在子窗口内加载，不触发 WebView2 COM 层 NewWindowRequested**，
// 从而避免"用户手敲搜索框 → 表单跨域提交到 login.1688.com → 冒新窗"的最大触发源。
// keyword 为空时回退到 1688 首页（供登录）。category 取值：factory/supplier/industry/offer。
func (e *windowEngine) OpenSearch(ctx context.Context, keyword, category string) error {
	url := ""
	if keyword != "" {
		url = e.searchURL(CollectOptions{Keyword: keyword, Category: category})
	}
	return e.OpenWindow(ctx, url)
}

// startBridgeLoop 启动 installBridge 持久注入循环（幂等：已在跑则不重复 spawn）。
func (e *windowEngine) startBridgeLoop() {
	e.bridgeMu.Lock()
	defer e.bridgeMu.Unlock()
	if e.bridgeRunning {
		return // 已有循环在跑
	}
	// 旧的 stop channel 可能已被 close 过，重建一个新的一供本循环使用
	select {
	case <-e.bridgeStop:
		// 已关闭，换新的
		e.bridgeStop = make(chan struct{})
	default:
		// 仍在用，复用
	}
	e.bridgeRunning = true
	go func() {
		defer func() {
			e.bridgeMu.Lock()
			e.bridgeRunning = false
			e.bridgeMu.Unlock()
		}()
		e.installBridge()
	}()
}

// OpenLogin 打开子窗口到 1688 首页（供用户登录）。
func (e *windowEngine) OpenLogin() error {
	return e.OpenWindow(context.Background(), "https://www.1688.com/")
}

// CloseLogin 关闭子窗口。
func (e *windowEngine) CloseLogin() {
	e.CloseWindow()
}

// diagnoseJS 读 window.__ltoolsCollectStatus（诊断探针），fetch POST 到 /c1688/diag。
// Go 端 CollectCurrentPage 第一次超时后注入它，把"为什么超时"变成日志可查（root cause 定位）。
// 不受 CSP connect-src 限制（127.0.0.1 兜底走 sendBeacon / Image）。
const diagnoseJS = `
(function(){
  try {
    var status = window.__ltoolsCollectStatus || null;
    var target = (window.__ltoolsCollectURL || '').replace(/\/c1688\/html$/, '/c1688/diag');
    var body = JSON.stringify({
      status: status,
      bridgeInstalled: !!window.__ltoolsBridgeInstalled,
      collectFn: typeof window.__ltoolsCollect,
      collectSuppliersFn: typeof window.__ltoolsCollectSuppliers,
      url: location.href,
      readyState: document.readyState,
      ts: Date.now(),
    });
    if (navigator.sendBeacon) {
      var blob = new Blob([body], { type: 'application/json' });
      if (navigator.sendBeacon(target, blob)) return;
    }
    if (window.fetch) {
      fetch(target, { method: 'POST', headers: { 'Content-Type': 'text/plain' }, body: body, keepalive: true }).catch(function(){});
    }
  } catch (e) {}
})();
`

// bridgeJS 注入到子窗口的脚本。做两件事（均按文档幂等）：
//
//  1. 数据回传：挂 window.__ltoolsCollect()，Go 端 CollectCurrentPage 调它把当前页
//     outerHTML 经 Wails 事件 c1688:page-html 发回。
//  2. 导航拦截（强化：事件 + 原型 + window.open 三层全拦）：把"会开新窗口 / 跳到当前窗口外"的
//     意图**全部**强制改成**当前窗口内跳转**，降低 WebView2 COM 层 NewWindowRequested 触发频率
//     （真正的兜底关窗靠 Go 端 OnCreate）：
//     - 捕获阶段 document click：拦所有 a[href]，target=_blank/_parent/_top 或跨域外链 → preventDefault + location.href。
//     - 捕获阶段 document submit：拦 form target=_blank/_parent/_top → preventDefault + location.href。
//     - 原型级拦截（编程式 a.click()/form.submit() 不产生 document 捕获事件，document 拦不到）：
//       重写 HTMLAnchorElement.prototype.click / HTMLFormElement.prototype.submit，开新窗意图 → location 跳转。
//     - 劫持 window.open（Object.defineProperty + configurable:false 让页面无法还原）：
//       url 非空 → 当前窗口 location 跳转；否则返回 fake window。同时劫持 document.defaultView.open。
//
// 幂等标记：window.__ltoolsBridgeInstalled。因为 1688 搜索/翻页是整页导航（新文档销毁 JS），
// Go 端 installBridge 循环会周期性重注入（80ms）；脚本自身在文档 loading 阶段挂 DOMContentLoaded 钩子，
// 把"整页导航后 hook 空窗"从 80ms 压到 ~0，使"导航完成瞬间点 _blank 链接/翻页"也能当场重定向。
const bridgeJS = `
(function(){
  // ---- 幂等：本文档已装则直接返回 ----
  if (window.__ltoolsBridgeInstalled) return;
  window.__ltoolsBridgeInstalled = true;

  // ---- 1. 数据回传 ----
  // 外部 1688 页没有 window.ltools（Wails 不注入外部站），且 ExecJS 是 void 读不回结果。
  // 主通道：fetch POST 到 Go 端本地回环（window.__ltoolsCollectURL，由 install 循环注入），
  // https 页向 http 127.0.0.1 发 body-only fetch 允许；响应不读（规避 CORS 读响应限制）。
  // 兜底通道：若有 window.ltools（内部页 / dev），走 Wails 事件；再退 parent.postMessage。
  // CSP 兜底：sendBeacon（items 64KB 内）/ Image src（不受 connect-src 限制）。
  window.__ltoolsCollect = function() {
    // 诊断信息挂到 window，Go 端超时后 ExecJS 探针读它（diagnoseJS）
    try {
      var itemsForStatus = [];
      try { if (window.__ltoolsSuppliers) { itemsForStatus = JSON.parse(window.__ltoolsSuppliers) || []; } } catch (e) {}
      window.__ltoolsCollectStatus = {
        ts: Date.now(),
        hasFn: typeof window.__ltoolsCollectSuppliers === 'function',
        itemsCount: (itemsForStatus || []).length,
        url: location.href,
        readyState: document.readyState,
        target: window.__ltoolsCollectURL || '',
      };
    } catch (e) {}
    try {
      // 取数：优先用 suppliersJS 已算好的 window.__ltoolsSuppliers（结构化 JSON），
      // 没有则回退整页 HTML（Go 端 parseHTML 兜底）。
      if (typeof window.__ltoolsCollectSuppliers === 'function') {
        try { window.__ltoolsCollectSuppliers(); } catch (e) {}
      }
      let items = [];
      try {
        if (window.__ltoolsSuppliers) {
          items = JSON.parse(window.__ltoolsSuppliers) || [];
        }
      } catch (e) { items = []; }
      const html = document.documentElement.outerHTML || "";
      const payload = { url: location.href, title: document.title, items: items, html: html };
      const target = window.__ltoolsCollectURL;
      if (target && window.fetch) {
        // body-only：不读 response（避免 CORS 读响应报错），送达即成功。
        // Content-Type: text/plain → 简单请求，不触发 preflight。
        // CSP 兜底：fetch 被 CSP connect-src 拦截（127.0.0.1 不在白名单）时走 sendBeacon（items）/
        // Image（items 序列化为 query），两者都不受 connect-src 限制。
        const doFallback = function() {
          try {
            // sendBeacon：items JSON 通常 < 64KB，够用；html 太大放不进，仅回传 items。
            if (navigator.sendBeacon) {
              const blob = new Blob([JSON.stringify({ url: location.href, items: items })], { type: 'application/json' });
              navigator.sendBeacon(target.replace(/\/c1688\/html$/, '/c1688/html') , blob);
            }
          } catch (e) {}
          try {
            // Image 兜底：把 items 塞进 query（64KB 内，超长会被截断），服务端读 query 也能用。
            const img = new Image();
            const qs = encodeURIComponent(JSON.stringify({ url: location.href, items: items })).slice(0, 60000);
            img.src = target + '?b=' + qs;
          } catch (e) {}
        };
        try {
          fetch(target, {
            method: 'POST',
            headers: { 'Content-Type': 'text/plain' },
            body: JSON.stringify(payload),
            keepalive: true,
          }).catch(doFallback);
        } catch (e) { doFallback(); }
        return 'fetch';
      }
      if (window.ltools && window.ltools.Events && window.ltools.Events.Emit) {
        window.ltools.Events.Emit('c1688:page-html', payload);
        return 'sent';
      }
      try { parent.postMessage(payload, '*'); } catch(e) {}
      return 'post';
    } catch (err) { return 'err:' + err; }
  };

  // ---- 2. 导航拦截（强化：事件 + 原型 + window.open 三层全拦）----
  const is1688 = function(u){
    if (!u) return true;
    try { return /(^|\\.)1688\\.com$/i.test(new URL(u, location.href).hostname); } catch(e){ return true; }
  };
  // 只有"会开新窗口 / 跳到当前窗口外"的才拦；同域普通链接/表单放行（保持 1688 正常点击/翻页）
  const willOpenNew = function(target, href){
    const t = (target || '').toLowerCase();
    if (t === '_blank' || t === '_parent' || t === '_top') return true;
    if (href && !is1688(href) && /^https?:/i.test(href)) return true; // 跨域外链
    return false;
  };
  const fakeWin = function(){
    return { close:function(){}, focus:function(){}, closed:false, location:{ href:"" }, document:{ write:function(){}, close:function(){} } };
  };
  // 统一处理"开新窗"意图：url 非空 → 当前窗口跳转；否则返回 fake window
  const openHook = function(url, name, features){
    if (typeof url === 'string' && url.length && url !== 'about:blank' && !/^about:/i.test(url)) {
      try { window.location.href = url; } catch(e) {}
      return fakeWin();
    }
    return fakeWin();
  };

  // (a) 捕获阶段拦 <a> 点击：开新窗 / 跳出 1688 → 改当前窗口
  document.addEventListener('click', function(ev){
    try {
      const a = ev.target && ev.target.closest ? ev.target.closest('a[href]') : null;
      if (!a) return;
      const tgt = (a.getAttribute('target') || '').toLowerCase();
      const href = a.getAttribute('href') || a.href;
      if (!href || href === '#' || href === 'javascript:;') return;
      if (willOpenNew(tgt, href)) {
        ev.preventDefault();
        ev.stopPropagation();
        try { window.location.href = href; } catch(e) {}
      }
    } catch(e) {}
  }, true);

  // (b) 捕获阶段拦 <form target=_blank|_parent|_top> 提交 → 改当前窗口
  document.addEventListener('submit', function(ev){
    try {
      const f = ev.target;
      if (!f || !f.tagName || f.tagName !== 'FORM') return;
      const tgt = (f.getAttribute('target') || '').toLowerCase();
      const action = f.getAttribute('action') || '';
      if (willOpenNew(tgt, action)) {
        if (action && action !== '#') {
          ev.preventDefault();
          ev.stopPropagation();
          try { window.location.href = action; } catch(e) {}
        }
      }
    } catch(e) {}
  }, true);

  // (c) 原型级拦截：编程式 a.click() / form.submit() 不触发 document 捕获事件，
  //     重写原型方法，开新窗意图 → 改 location 跳转（保持"始终一个子窗口"）
  const anchorClick = HTMLAnchorElement.prototype.click;
  if (anchorClick) {
    HTMLAnchorElement.prototype.click = function(){
      try {
        const a = this;
        if (a && a.tagName === 'A') {
          const tgt = (a.getAttribute('target') || '').toLowerCase();
          const href = a.getAttribute('href') || a.href;
          if (href && href !== '#' && href !== 'javascript:;' && willOpenNew(tgt, href)) {
            try { window.location.href = href; } catch(e) {}
            return;
          }
        }
      } catch(e) {}
      return anchorClick.apply(this, arguments);
    };
  }
  const formSubmit = HTMLFormElement.prototype.submit;
  if (formSubmit) {
    HTMLFormElement.prototype.submit = function(){
      try {
        const f = this;
        if (f && f.tagName === 'FORM') {
          const tgt = (f.getAttribute('target') || '').toLowerCase();
          const action = f.getAttribute('action') || '';
          if (action && action !== '#' && willOpenNew(tgt, action)) {
            try { window.location.href = action; } catch(e) {}
            return;
          }
        }
      } catch(e) {}
      return formSubmit.apply(this, arguments);
    };
  }

  // (d) 劫持 window.open：Object.defineProperty + configurable:false 让页面无法还原。
  //     同时劫持 document.defaultView.open（某些压缩 bundle 走这条读原始 open）
  const defineOpen = function(obj){
    try {
      Object.defineProperty(obj, 'open', {
        value: openHook,
        writable: false,
        configurable: false,
      });
    } catch(e) {}
  };
  defineOpen(window);
  if (document && document.defaultView) { try { defineOpen(document.defaultView); } catch(e) {} }

  // (e) 文档 loading 阶段挂 DOMContentLoaded 钩子：整页导航后新文档 readyState=loading 时
  //     本文档上下文销毁，Go 端 80ms 轮询会重注入；若 hook 注入早于 load，此钩子确保
  //     load 完成前 hook 已就位（把"导航后空窗"从 80ms 压到 ~0）
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', function(){ /* hook 已在本文档作用域装好 */ }, { once: true });
  }
})();
`

// lightColorSchemeJS 强制子窗口浅色渲染，抑制 WebView2 对未声明 color-scheme 页面的
// 自动暗色化（auto-darken）——这是"1688 首页偏暗"的根因。幂等：已注入则不重复。
// 整页导航销毁 JS 后由 install 循环（80ms）重注，覆盖"导航完成瞬间"的空窗。
const lightColorSchemeJS = `
(function(){
  try {
    var rs = document.documentElement.style;
    if (rs) { rs.colorScheme = 'light'; }
    var meta = document.querySelector('meta[name="color-scheme"]');
    if (!meta) {
      meta = document.createElement('meta');
      meta.name = 'color-scheme';
      meta.content = 'light';
      document.head.appendChild(meta);
    } else if (meta.content !== 'light') {
      meta.content = 'light';
    }
    // 兜底：给 html/body 显式浅色，防 1688 某些容器用 dark 变量
    var s = document.getElementById('__ltools_light_css__');
    if (!s) {
      s = document.createElement('style');
      s.id = '__ltools_light_css__';
      s.textContent = ':root{color-scheme:light;} html,body{background:#fff;}';
      document.head.appendChild(s);
    }
  } catch (e) {}
})();
`

// suppliersJS 在 1688 结果页里直接取"供应商列表"结构化数据，归一化成
// [{name,phone,address,main,level,url}] 后存到 window.__ltoolsSuppliers。
// 三层策略（命中即返回，比 Go 端对 outerHTML 跑正则准得多）：
//  1. 活数据对象：window.__INITIAL_DATA__ / __INITIAL_STATE__ / __data__ /
//     __pageData__ / __NEXT_DATA__，递归找"含公司名 + (电话|地址|旺旺)"的供应商数组。
//  2. DOM 卡片启发式：当前文档里"卡片容器内同时出现公司名 + 电话/地址/旺旺"的卡片。
//  3. 数据对象候选键对齐 Go 的 collectJSON（companyName/shopName/… + mobile/phone/…）。
// 结果写 window.__ltoolsSuppliers（JSON 字符串），由 __ltoolsCollect 读出来随回传带回。
// 整页导航销毁 window 后由 install 循环（80ms）重注，保证"导航完成瞬间"也能取数。
const suppliersJS = `
(function(){
  function normName(s){ if(!s) return ""; s=String(s).trim(); if(s.length<2||s.length>80) return ""; return s; }
  function normPhone(s){ if(s==null) return ""; s=String(s); var m=s.replace(/\\D/g,''); if(m.length>=7 && m.length<=15) return s.replace(/[^0-9+\\-() ]/g,''); if(/^\\d{11}$/.test(m)) return m; return s; }
  function pick(obj, keys){ if(!obj) return ""; for(var i=0;i<keys.length;i++){ var v=obj[keys[i]]; if(v!=null && String(v).trim()!=="") return String(v).trim(); } return ""; }

  // ---- 1. 从活数据对象里递归找供应商 ----
  function fromData(root){
    var out=[]; var seen={};
    var NAME_KEYS=["companyName","shopName","sellerName","name","fullName","companyNameStr","title"];
    var PHONE_KEYS=["mobile","phone","tel","telephone","contactPhone","mobileNo","contactMobile","telNo"];
    var ADDR_KEYS=["address","fullAddress","addressDetail","province","city","town","provinceName","cityName","townName","areaName"];
    function isSupplier(m){
      var hasName = NAME_KEYS.some(function(k){return m[k]!=null && String(m[k]).trim()!=="";});
      var hasPhone = PHONE_KEYS.some(function(k){return m[k]!=null && String(m[k]).trim()!=="";});
      var hasAddr = ADDR_KEYS.some(function(k){return m[k]!=null && String(m[k]).trim()!=="";});
      var hasWangwang = !!(m["loginId"]||m["ww"]||m["wangwang"]);
      return hasName && (hasPhone||hasAddr||hasWangwang);
    }
    function walk(v, depth){
      if(!v || depth>12) return;
      if(Array.isArray(v)){ for(var i=0;i<v.length;i++) walk(v[i], depth+1); return; }
      if(typeof v==="object"){
        if(isSupplier(v)){
          var name=normName(pick(v,NAME_KEYS));
          var phone=normPhone(pick(v,PHONE_KEYS));
          var addrParts=[pick(v,["province","provinceName"]),pick(v,["city","cityName"]),pick(v,["town","townName"]),pick(v,["address","fullAddress","addressDetail","areaName"])];
          var address=addrParts.filter(Boolean).join(" ");
          var main=pick(v,["mainProduct","mainCategory","categoryName","mainBusiness","description","category"]);
          var level=pick(v,["level","strengthTag","creditLevel","years","companyLevel","medalLevel","creditYears"]);
          var url=pick(v,["url","link","shopUrl","winportUrl","shopLink","detailUrl","offerUrl"]);
          var key=name+"||"+address;
          if((name||phone||address) && !seen[key]){ seen[key]=1;
            out.push({name:name,phone:phone,address:address,main:main,level:level,url:url});
          }
          // 不深入供应商内部（避免把无关子对象误判）
          return;
        }
        for(var k in v) if(v.hasOwnProperty(k)) walk(v[k], depth+1);
      }
    }
    var candidates=[];
    ["__INITIAL_DATA__","__INITIAL_STATE__","__data__","__pageData__","__NEXT_DATA__"].forEach(function(n){
      try{ if(window[n] && typeof window[n]==="object") candidates.push(window[n]); }catch(e){}
    });
    // 也尝试全局 data 挂在 document 的 data-* / script[type=json]
    var sels=document.querySelectorAll('script[type="application/json"],script[id*="data"],script[class*="data"]');
    for(var i=0;i<sels.length;i++){ try{ var j=JSON.parse(sels[i].textContent); if(j) candidates.push(j); }catch(e){} }
    for(var c=0;c<candidates.length && out.length===0;c++) walk(candidates[c],0);
    return out;
  }

  // ---- 2. DOM 卡片启发式 ----
  function fromDOM(){
    var out=[]; var seen={};
    // 公司名候选标签
    var nameSel='a[class*="company"],[class*="company-name"],[class*="shop-name"],[class*="supplier-name"],[class*="text-company"],[class*="b2b-company"]';
    var names=document.querySelectorAll(nameSel);
    names.forEach(function(el){
      var card=el.closest('[class*="item"],[class*="card"],[class*="module"],li,div[class*="offer"]')||el.parentElement;
      if(!card) return;
      var name=(el.textContent||"").replace(/\\s+/g,' ').trim();
      if(name.length<2||name.length>80) name="";
      var phone="";
      var telA=card.querySelector('a[href^="tel:"]'); if(telA){ phone=telA.getAttribute('href').replace(/^tel:/,''); }
      if(!phone){ var m=(card.innerText||"").match(/(1[3-9]\\d{9}|\\d{3,4}[- ]?\\d{7,8})/); if(m) phone=m[0]; }
      var addr="";
      var addrEl=card.querySelector('[class*="location"],[class*="address"],[class*="town"],[class*="province"],[class*="area"]');
      if(addrEl){ addr=(addrEl.textContent||"").replace(/\\s+/g,' ').trim(); }
      var main=""; var mainEl=card.querySelector('[class*="main"],[class*="category"],[class*="product"]'); if(mainEl) main=(mainEl.textContent||"").trim().slice(0,60);
      var url=""; var shopA=card.querySelector('a[href*="detail.1688.com"],a[href*=".1688.com"]'); if(shopA) url=shopA.href||shopA.getAttribute('href')||"";
      var key=name+"||"+addr;
      if((name||phone||addr) && !seen[key]){ seen[key]=1; out.push({name:name,phone:phone,address:addr,main:main,level:"",url:url}); }
    });
    return out;
  }

  // ---- 0. 1688 结果页专用（真实页面结构：window.data.offerresultData / 哈希 css-modules 类）----
  // 命中条件：window.data 存在（1688 搜索结果页）；从 window.data 递归找供应商数组。
  // 1688 结果页卡片没有电话（只有店名/地址/店铺 URL），电话列留空是预期；
  // 若要电话，需走详情页（见 service.go 的双窗编排：detail 窗口逐个访问 item.Url）。
  function from1688Result(){
    var out=[]; var seen={};
    var NAME_KEYS=["shopName","companyName","companyNameStr","sellerName","loginId","memberId","name"];
    var ADDR_KEYS=["province","provinceName","city","cityName","town","townName","address","fullAddress","areaName","addressDetail"];
    var URL_KEYS=["shopUrl","winportUrl","companyUrl","url","link","offerUrl"];
    function pushItem(name, phone, address, main, level, shopUrl){
      name=normName(name); phone=normPhone(phone);
      address=address||""; main=main||""; level=level||""; shopUrl=shopUrl||"";
      var key=name+"||"+address+"||"+shopUrl;
      if(!key || key==="||") return;
      if(seen[key]) return;
      seen[key]=1;
      out.push({name:name,phone:phone,address:address,main:main,level:level,url:shopUrl});
    }
    // 递归找 window.data 里"含 NAME_KEYS 的项"
    var dataObj = (typeof window.data === "object" && window.data) ? window.data : null;
    if(dataObj){
      var walk=function(v, depth){
        if(!v || depth>10) return;
        if(Array.isArray(v)){ for(var i=0;i<v.length;i++) walk(v[i], depth+1); return; }
        if(typeof v==="object"){
          var hasName = NAME_KEYS.some(function(k){ return v[k]!=null && String(v[k]).trim()!==""; });
          if(hasName){
            var name=normName(pick(v,NAME_KEYS));
            var addrParts=[pick(v,ADDR_KEYS)];
            var address=addrParts.filter(Boolean).join(" ");
            var main=pick(v,["mainProduct","mainCategory","categoryName","mainBusiness","description"]);
            var level=pick(v,["level","strengthTag","creditLevel","years","companyLevel","medalLevel","creditYears"]);
            var url=pick(v,URL_KEYS);
            if(name || address){ pushItem(name,"",address,main,level,url); }
            // 供应商对象内部不再深入（避免误吞子对象）
            return;
          }
          for(var k in v) if(v.hasOwnProperty(k)) walk(v[k], depth+1);
        }
      };
      walk(dataObj, 0);
    }
    // 兜底：DOM 层（1688 结果页 css-modules 哈希类）
    if(out.length===0){
      var nameSel='[class^="shopRow--"] a[href*=".1688.com"][title], a[href*=".1688.com"][title]';
      var cards=document.querySelectorAll('[class^="offerList--"], [class^="gridCell--"], [class*="offerCard--"]');
      cards.forEach(function(card){
        var nameEl=card.querySelector('[class^="shopRow--"] a[title], a[title]');
        var name = nameEl ? (nameEl.getAttribute("title")||nameEl.textContent||"").trim() : "";
        name=normName(name);
        var addr="";
        var addrEl=card.querySelector('[class*="location"],[class*="addr"],[class*="town"],[class*="province"]');
        if(addrEl){ addr=(addrEl.textContent||"").replace(/\\s+/g,' ').trim(); }
        var urlEl=card.querySelector('a[href*=".1688.com"]');
        var url = urlEl ? (urlEl.href||urlEl.getAttribute("href")||"") : "";
        var mainEl=card.querySelector('[class*="title"],[class*="product"],[class*="main"]');
        var main = mainEl ? (mainEl.textContent||"").trim().slice(0,60) : "";
        if(name || addr || url){ pushItem(name,"",addr,main,"",url); }
      });
    }
    return out;
  }

  function collect(){
    var list=[];
    // 优先 1688 结果页专用结构（window.data / 哈希类），命中即用
    try{ list=from1688Result(); }catch(e){}
    if(list.length===0){ try{ list=fromData(window); }catch(e){} }
    if(list.length===0){ try{ list=fromDOM(); }catch(e){} }
    window.__ltoolsSuppliers = JSON.stringify(list||[]);
    window.__ltoolsSuppliersCount = (list||[]).length;
    return list;
  }
  window.__ltoolsCollectSuppliers = collect;
  collect();
})();
`

// scrollLoadMoreJS 滚一屏触发懒加载，并报告"是否到底"。
// 每次调用滚一屏（scrollTop=scrollHeight），若内容高度不增长则 noGrow++。
// 到底判定：noGrow>=2（连续 2 次无新增）或 scrolls>6（最多 6 屏防死循环）。
// 结果：window.__ltoolsScrollDone = 1 表示"到底了，不用再滚"，否则 0。
const scrollLoadMoreJS = `
(function(){
  var sc=document.scrollingElement||document.documentElement||document.body;
  if(!sc) return;
  window.__ltoolsLastHeight=window.__ltoolsLastHeight||0;
  window.__ltoolsNoGrow=window.__ltoolsNoGrow||0;
  window.__ltoolsScrolls=window.__ltoolsScrolls||0;
  sc.scrollTop=sc.scrollHeight;
  var newH=(document.body?document.body.scrollHeight:0)||sc.scrollHeight;
  if(newH<=window.__ltoolsLastHeight){ window.__ltoolsNoGrow++; } else { window.__ltoolsNoGrow=0; window.__ltoolsLastHeight=newH; }
  window.__ltoolsScrolls++;
  var atBottom = window.__ltoolsNoGrow>=2 || window.__ltoolsScrolls>6;
  window.__ltoolsScrollDone = atBottom?1:0;
})();
`

// clickNextPageJS 点「下一页」。多策略定位，命中即点（走原型 click，被 bridgeJS 拦为当前窗跳转）。
// 返回 1=点成功 / 0=没找到下一页。
const clickNextPageJS = `
(function(){
  function txt(el){ return (el.textContent||"").replace(/\\s+/g,'').trim(); }
  function isDisabled(a){ return a.hasAttribute('disabled')||/(arrowDisabled|disabled_)/.test(a.className||''); }
  function clickAnchor(a){
    if(!a) return false;
    if(isDisabled(a)) return false;
    try{ if(a.click) { a.click(); } else { a.dispatchEvent(new MouseEvent('click',{bubbles:true})); } }catch(e){ try{ location.href=a.href; }catch(e2){} }
    return true;
  }
  // 策略 1（1688 结果页）：[class^="pagination--"] 内的 nextArrow（禁用时含 arrowDisabled 类 → 到底了）
  try{
    var pg=document.querySelector('[class^="pagination--"]');
    if(pg){
      var next=pg.querySelector('[class*="nextArrow"]:not([class*="arrowDisabled"])');
      if(next){
        if(isDisabled(next)){ return 0; } // 到底了 / 禁用
        if(clickAnchor(next)) return 1;
      }
      // nextArrow 不存在或 disabled：退化为"比当前页码大的 pageItem--* 数字项里最小的那个"
      var items=pg.querySelectorAll('[class^="pageItem--"], [class*="pageItem--"]');
      var cur=0;
      items.forEach(function(it){
        if(/active|selected|current/i.test(it.className||'')){
          var m=(it.textContent||'').match(/(\\d+)/);
          if(m) cur=parseInt(m[1],10);
        }
      });
      var candidates=[];
      items.forEach(function(it){
        var m=(it.textContent||'').match(/(\\d+)/);
        if(m){
          var n=parseInt(m[1],10);
          if(n>cur && !isDisabled(it)) candidates.push(n);
        }
      });
      if(candidates.length>0){
        candidates.sort(function(a,b){return a-b;});
        var target=candidates[0];
        var tEl=null;
        items.forEach(function(it){
          var m=(it.textContent||'').match(/(\\d+)/);
          if(m && parseInt(m[1],10)===target) tEl=it;
        });
        if(tEl && clickAnchor(tEl)){ return 1; }
      }
      // 找到 pagination 但无法翻页（全 disabled / 无候选）→ 已到底
      return 0;
    }
  }catch(e){}
  // 策略 2：文本 = 下一页 / » / 下页
  var anchors=document.querySelectorAll('a');
  for(var i=0;i<anchors.length;i++){
    var t=txt(anchors[i]);
    if(t==='下一页'||t==='下页'||t==='»'||t==='›'||t==='>') { if(clickAnchor(anchors[i])) return 1; }
  }
  // 策略 3：pagination 容器里文本=下一页
  var pg2=document.querySelectorAll('[class*="pagination"] a,[class*="pager"] a');
  for(var j=0;j<pg2.length;j++){
    var tt=txt(pg2[j]);
    if(tt==='下一页'||tt==='»'){ if(clickAnchor(pg2[j])) return 1; }
  }
  // 策略 4：URL 翻页参数（pageNum / page）+1
  try{
    var u=new URL(location.href);
    var changed=false;
    if(u.searchParams.has('pageNum')){ u.searchParams.set('pageNum', parseInt(u.searchParams.get('pageNum'),10)+1); changed=true; }
    else if(u.searchParams.has('page')){ u.searchParams.set('page', parseInt(u.searchParams.get('page'),10)+1); changed=true; }
    if(changed){ location.href=u.toString(); return 1; }
  }catch(e){}
  return 0;
})();
`

// installBridge 在子窗口里**持久**注入 bridgeJS（数据回传 + 导航拦截）。
// Wails 子窗口是独立 WebView；bridge 通过 Wails 全局事件总线回传。
// 用 currentWindow() 实时取窗口句柄（而非 e.win 字段），避免多实例 / 过期引用。
// 循环：每 80ms 注入一次（幂等），直到 closeBridgeLoop 触发（CloseWindow）。
// 因 1688 搜索/翻页是整页导航会销毁 JS 上下文，周期性重注入是导航 hook 能"跨导航存活"的关键。
func (e *windowEngine) installBridge() {
	if e.app == nil {
		return
	}
	// missingTicks：子窗口连续不存在的 tick 数；超过 125（=10s @80ms）视为窗口已关且不会立即重开，退出循环。
	// 窗口存在时归零。这样循环既能在"整页导航间隙"保持存活（窗口一直在），又能在"窗口关闭"后退出。
	// 轮询间隔压到 80ms：配合 bridgeJS 的 DOMContentLoaded 钩子，把"整页导航后 JS 上下文销毁 →
	// hook 恢复"的空窗从 150ms 压到 ~0，大幅降低 WebView2 COM 层 NewWindowRequested 在空窗内被触发。
	const maxMissingTicks = 125
	missingTicks := 0
	for {
		// 可被 CloseWindow 终止
		select {
		case <-e.bridgeStop:
			return
		default:
		}
		w, ok := e.currentWindow()
		if !ok {
			missingTicks++
			if missingTicks > maxMissingTicks {
				return
			}
		} else {
			missingTicks = 0
			// 先注入回传目标 URL（整页导航会销毁 window，需随 bridge 一起周期重注），再注 bridge。
			w.ExecJS("window.__ltoolsCollectURL=" + e.jsStringLiteral(e.htmlReturnURL()) + ";")
			w.ExecJS(bridgeJS)
			// 取数脚本（__ltoolsCollectSuppliers + 立即跑一次），整页导航后重注。
			w.ExecJS(suppliersJS)
			// 重置滚动状态（每页导航后 scrollTop 归零，需要重新从头滚）
			w.ExecJS(`window.__ltoolsNoGrow=0;window.__ltoolsScrolls=0;window.__ltoolsLastHeight=0;`)
			// 防暗色：WebView2 在 PrefLight 未声明时可能对页面做自动暗色化（auto-darken），
			// 让 1688 白底页变灰暗。这里强制 :root color-scheme=light + 注入 <meta>，
			// 整页导航后随 bridge 一起重注（幂等）。
			w.ExecJS(lightColorSchemeJS)
		}
		time.Sleep(80 * time.Millisecond)
	}
}

// closeBridgeLoop 终止 installBridge 持久循环（CloseWindow 时调用）。
func (e *windowEngine) closeBridgeLoop() {
	e.bridgeMu.Lock()
	defer e.bridgeMu.Unlock()
	if e.bridgeStop != nil {
		select {
		case <-e.bridgeStop:
		default:
			close(e.bridgeStop)
		}
	}
}

// collectItems 通过 WebView2 ExecuteScriptWithResult（chromium.EvalWithResult → ExecJSResult）
// 同步读回当前页的结构化供应商数据（JSON 字符串），不再依赖 127.0.0.1 HTTP 回传。
// 这是"回传 HTML 超时"问题的根治方案：JS 取数结果直接 COM 读回，不经过外部 fetch。
//
// 流程：对子窗口 ExecJS(suppliersJS) 让它在当前文档里算好 window.__ltoolsSuppliers（JSON 串），
// 再 ExecJSResult 读回该串 → Unmarshal 成 []Supplier。
// 返回 nil 表示取不到（窗口未开 / ExecJSResult 超时 / 非结果页）。
func (e *windowEngine) collectItems(ctx context.Context) []Supplier {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
	}
	w, ok := e.currentWindow()
	if !ok {
		return nil
	}
	// 0) 先确认页面稳定（document.readyState === "complete"），避免在导航/渲染期间
	//    调 ExecuteScriptWithResult 碰 RESOURCE_NOT_IN_CORRECT_STATE（WebView2 内部状态不对）。
	//    最多等 5s（10 × 500ms），超时仍尝试（1688 页 readyState 通常很快 complete）。
	ready, readyErr := w.ExecJSResult(`document.readyState`, 3*time.Second)
	if readyErr == nil && strings.TrimSpace(ready) != `"complete"` {
		if e.logf != nil {
			e.logf(fmt.Sprintf("[collectItems] 页面未 ready（readyState=%s），等 5s...", truncateStr(ready, 40)))
		}
		for i := 0; i < 10; i++ {
			if ctx != nil {
				select {
				case <-ctx.Done():
					return nil
				default:
				}
			}
			time.Sleep(500 * time.Millisecond)
			ready, readyErr = w.ExecJSResult(`document.readyState`, 3*time.Second)
			if readyErr == nil && strings.TrimSpace(ready) == `"complete"` {
				break
			}
		}
	}
	// 1) 先让取数脚本在当前文档跑一遍（幂等，整页导航后由 install 循环重注兜底）。
	//    suppliersJS 末尾 collect() 会把结果写到 window.__ltoolsSuppliers。
	// 2) 同步读回（COM 回调）。读回空 / 失败时重试（2s × 3 次，让 WebView2 从
	//    RESOURCE_NOT_IN_CORRECT_STATE 恢复 + install 循环重注 suppliersJS）。
	const maxAttempts = 3
	const retryDelay = 2 * time.Second
	var out string
	var err error
	var items []Supplier
	var uerr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if e.logf != nil {
				e.logf(fmt.Sprintf("[collectItems] 第 %d 次重试（%v 后）...", attempt+1, retryDelay))
			}
			time.Sleep(retryDelay)
			if ctx != nil {
				select {
				case <-ctx.Done():
					return nil
				default:
				}
			}
		}
		w.ExecJS(suppliersJS)
		out, err = w.ExecJSResult(`window.__ltoolsSuppliers || "[]"`, 5*time.Second)
		items, uerr = unmarshalSuppliers(out)
		if len(items) > 0 {
			break
		}
	}
	if err != nil {
		if e.logf != nil {
			// 诊断：区分三种根因
			// ① "ICoreWebView2_21 not supported" → 老 WebView2 运行时（QueryInterface 失败）
			// ② "EvalWithResult: timeout" → COM 回调线程模型问题（死锁/未派发）
			// ③ "RESOURCE_NOT_IN_CORRECT_STATE" → WebView2 内部状态不对（导航/DPI/双窗）
			e.logf(fmt.Sprintf("[ExecJSResult] 读回供应商数据失败（根因诊断，%d 次尝试）：%v",
				maxAttempts, err))
		}
		return nil
	}
	if uerr != nil {
		if e.logf != nil {
			e.logf(fmt.Sprintf("[ExecJSResult] 解析供应商 JSON 失败（根因：suppliersJS 没跑成功）：%v（raw=%s）",
				uerr, truncateStr(out, 200)))
		}
		return nil
	}
	if e.logf != nil {
		e.logf(fmt.Sprintf("[ExecJSResult] 读回 %d 条供应商", len(items)))
	}
	return items
}

// unmarshalSuppliers 把 JS 读回的 JSON 字符串解析成 []Supplier。
// 兼容两种形态：直接是数组 '[{...}]'；或外层包了对象（容错）。空/非 JSON 返回 (nil, nil)。
func unmarshalSuppliers(raw string) ([]Supplier, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" || raw == "[]" {
		return nil, nil
	}
	var items []Supplier
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		// 可能 JS 返回的是带外层对象的字符串，再试包一层
		var wrap struct {
			Items []Supplier `json:"items"`
		}
		if werr := json.Unmarshal([]byte(raw), &wrap); werr == nil {
			return wrap.Items, nil
		}
		return nil, err
	}
	return items, nil
}

// truncateStr 截断长字符串（日志用）。
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// CollectCurrentPage 对子窗口当前页执行提取，返回 []Supplier。
//
// 首选通道（同步读回，根治"回传 HTML 超时"）：collectItems() 走 WebView2
// ExecuteScriptWithResult，把 JS 取到的 items JSON 直接 COM 读回，不经 127.0.0.1。
// 兜底通道（HTTP 回传）：collectItems 拿不到时（旧 WebView2 运行时 / JS 上下文异常），
// 退回到 __ltoolsCollect → fetch POST 127.0.0.1:48217 的传统路径。
func (e *windowEngine) CollectCurrentPage(ctx context.Context, source string, logf func(string)) []Supplier {
	w, ok := e.currentWindow()
	if !ok {
		if logf != nil {
			logf("子窗口未打开，无法采集；请先点「打开 1688」")
		}
		return nil
	}

	// ---- 首选：同步读回（不依赖 HTTP 回传 / 不依赖外部 fetch）----
	if items := e.collectItems(ctx); len(items) > 0 {
		for i := range items {
			if items[i].Source == "" {
				items[i].Source = source
			}
			if items[i].Time == "" {
				items[i].Time = nowLocal()
			}
		}
		if logf != nil {
			logf(fmt.Sprintf("[ExecJSResult] 同步读回 %d 条供应商（url=%s）", len(items), e.currentURLForLog()))
		}
		return items
	}

	// ---- 兜底：HTTP 回传（__ltoolsCollect → fetch 127.0.0.1:48217）----
	if logf != nil {
		logf("同步读回未取到数据，回退 HTTP 回传通道（__ltoolsCollect）")
	}
	e.drainChannel()
	e.drainCollectReq()

	// 强制重注 bridgeJS + suppliersJS + __ltoolsCollectURL（不等 install 循环，
	// 覆盖"install 循环在 WebView2 bad state 时没注到"的场景）。等 500ms 让 JS 执行。
	w.ExecJS(`window.__ltoolsCollectURL=` + e.jsStringLiteral(e.htmlReturnURL()) + `;`)
	w.ExecJS(bridgeJS)
	w.ExecJS(suppliersJS)
	time.Sleep(500 * time.Millisecond)

	// 3 次重试：8s + 3s + 3s。第二次超时后注入 diagnoseJS 探针。
	for attempt := 0; attempt < 3; attempt++ {
		w.ExecJS(`if (window.__ltoolsCollect) window.__ltoolsCollect();`)
		if attempt == 1 {
			// 第二次超时后注入诊断探针（把 window.__ltoolsCollectStatus POST 到 /c1688/diag）
			w.ExecJS(diagnoseJS)
		}
		wait := 8 * time.Second
		if attempt != 0 {
			wait = 3 * time.Second
		}
		if p, done := e.collectReqWait(ctx, wait); done {
			return e.finishCollect(p, source, logf)
		}
	}
	if logf != nil {
		logf("子窗口回传 HTML 超时（3 次重试均失败）；请确认子窗口已加载 1688 页面且未被拦截（回传走 127.0.0.1:48217）。[diag] 日志见上。")
	}
	return nil
}

// currentURLForLog 取子窗口当前 URL（仅日志用，失败返回空）。
func (e *windowEngine) currentURLForLog() string {
	w, ok := e.currentWindow()
	if !ok {
		return ""
	}
	out, err := w.ExecJSResult(`location.href`, 2*time.Second)
	if err != nil {
		return ""
	}
	// location.href 读回是带引号的 JSON 字符串（如 "https://..."）
	out = strings.Trim(out, `"`)
	return out
}

// collectReqWait 阻塞等待 collectReq 或 pageHTMLCh（任一有值即返回），或超时 / ctx 取消。
// 返回 (payload, true) 表示拿到数据；(zero, false) 表示没拿到。
func (e *windowEngine) collectReqWait(ctx context.Context, d time.Duration) (pageHTMLPayload, bool) {
	select {
	case p := <-e.collectReq:
		return p, true
	case p := <-e.pageHTMLCh:
		return p, true
	case <-time.After(d):
		return pageHTMLPayload{}, false
	case <-ctx.Done():
		return pageHTMLPayload{}, false
	}
}

// finishCollect 把收到的载荷解析成供应商并记日志。
// 优先用子窗口 JS 取到的结构化 Items（最准）；Items 为空才回退 Go 端 parseHTML(HTML)。
func (e *windowEngine) finishCollect(p pageHTMLPayload, source string, logf func(string)) []Supplier {
	if len(p.Items) > 0 {
		// 补 source/time（JS 侧只给 name/phone/address/main/level/url）
		for i := range p.Items {
			if p.Items[i].Source == "" {
				p.Items[i].Source = source
			}
			if p.Items[i].Time == "" {
				p.Items[i].Time = nowLocal()
			}
		}
		if logf != nil {
			logf(fmt.Sprintf("子窗口 JS 取到 %d 条供应商（url=%s）", len(p.Items), p.URL))
		}
		return p.Items
	}
	if p.HTML == "" {
		if logf != nil {
			logf("子窗口回传 HTML 为空且无结构化数据（页面可能未加载 / 非结果页 / 被 baxia 拦截）")
		}
		return nil
	}
	if logf != nil {
		logf(fmt.Sprintf("JS 未取到结构化数据，回退 HTML 解析（url=%s, %d 字符）", p.URL, len(p.HTML)))
	}
	items := parseHTML(p.HTML, source, 0)
	if logf != nil {
		logf(fmt.Sprintf("HTML 解析到 %d 条", len(items)))
	}
	return items
}

// drainCollectReq 清空 collectReq 残留。
func (e *windowEngine) drainCollectReq() {
	for {
		select {
		case <-e.collectReq:
		default:
			return
		}
	}
}

// CollectAllPages 连续多页采集：当前页 + 用户手动翻页 N 次。
// 翻页由用户在子窗口里手动点「下一页」（拟人轨迹最真实），本方法只读当前页一次。
// 多页采集由前端循环调用 CollectCurrentPage 完成（用户每次手动翻页后再点一次）。
func (e *windowEngine) CollectAllPages(ctx context.Context, o CollectOptions, logf func(string)) []Supplier {
	return e.CollectCurrentPage(ctx, o.Keyword, logf)
}

// CloseWindow 关闭子窗口（按名字全局查，跨实例安全），并终止 bridge 注入循环。
func (e *windowEngine) CloseWindow() {
	e.closeBridgeLoop()
	if w, ok := e.currentWindow(); ok {
		w.Close()
	}
	e.win = nil
}

// IsWindowOpen 返回子窗口是否还开着（实时查全局 windows map）。
func (e *windowEngine) IsWindowOpen() bool {
	_, ok := e.currentWindow()
	return ok
}

// ScrollToBottom 在子窗口里滚动加载到底（触发懒加载），每 interval 滚一屏，
// 直到"到底"（连续 2 次无新增 或 超过 6 屏）。用于采当前页前把懒加载内容抓全。
//
// 现走 ExecJSResult 同步读回：先 ExecJS(scrollLoadMoreJS) 滚一屏（写 window.__ltoolsScrollDone），
// 再 ExecJSResult 读回该标记（"1"=到底 / "0"=还有）。读回失败退化为"固定滚 N 次"旧行为。
func (e *windowEngine) ScrollToBottom(interval time.Duration) {
	if interval <= 0 {
		interval = 1200 * time.Millisecond
	}
	for i := 0; i < 8; i++ {
		w, ok := e.currentWindow()
		if !ok {
			return
		}
		// 滚一屏（scrollLoadMoreJS 末尾写 window.__ltoolsScrollDone = atBottom?1:0）。
		w.ExecJS(scrollLoadMoreJS)
		// 同步读回"是否到底"，明确判定（替代旧"固定滚 6 次"猜测）。
		done := false
		if out, err := w.ExecJSResult(`window.__ltoolsScrollDone ? 1 : 0`, 2*time.Second); err == nil {
			done = strings.TrimSpace(out) == "1"
		}
		if done {
			return
		}
		if i >= 7 {
			break
		}
		time.Sleep(interval)
	}
}

// ClickNextPage 在子窗口里点「下一页」。返回 true 表示点了（页面将跳转），
// false 表示没找到下一页（已到底 / 风控拦截页）。
// 定位策略见 clickNextPageJS（pagination nextArrow → 文本 → URL 参数 +1）。
//
// 现走 ExecJSResult 同步读回 clickNextPageJS 的返回值（"1"/"0"），不再"盲点 + 指纹猜测"：
// 读到 "1" = 真点了；读到 "0" = 明确到底/没下一页，调用方据此停止翻页（替代指纹猜测）。
// 读回失败（超时/旧运行时）时退化为"盲点"并返回 true（保持旧行为，让调用方靠指纹兜底）。
func (e *windowEngine) ClickNextPage() bool {
	w, ok := e.currentWindow()
	if !ok {
		return false
	}
	out, err := w.ExecJSResult(clickNextPageJS, 5*time.Second)
	if err != nil {
		// 读不回（超时 / 旧 WebView2 运行时无 ICoreWebView2_21）→ 退化为盲点，保持旧语义。
		if e.logf != nil {
			e.logf(fmt.Sprintf("[ClickNextPage] ExecJSResult 读回失败，退化为盲点：%v", err))
		}
		w.ExecJS(clickNextPageJS)
		return true
	}
	// clickNextPageJS 返回 1/0（数字），GetResultAsJson 读回为 "1"/"0"。
	trimmed := strings.TrimSpace(out)
	if trimmed == "1" {
		return true
	}
	// "0"（到底 / 没下一页）或其他 → 明确没翻成。
	return false
}

// SetURL 把子窗口导航到指定 URL（用于双窗编排：detail 窗逐个访问 item.Url）。
// 返回 false 表示窗口未开。导航后由 installBridge 循环（80ms）重注 bridge，
// 等待"详情页加载完"由调用方 time.Sleep 完成。
func (e *windowEngine) SetURL(ctx context.Context, url string) bool {
	w, ok := e.currentWindow()
	if !ok {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	default:
	}
	if url != "" {
		w.SetURL(url)
	}
	return true
}

// DumpPageHTML 把子窗口当前页 HTML 落盘到 diagDir（诊断用），返回文件路径。
// 走 ExecJSResult（COM 同步读回 document.documentElement.outerHTML），不依赖 127.0.0.1 HTTP
// 回传（外部 1688 页 fetch 被风控拦 → 老路径必超时）。
func (e *windowEngine) DumpPageHTML(tag string) string {
	w, ok := e.currentWindow()
	if !ok || e.diagDir == "" {
		return ""
	}
	html, err := w.ExecJSResult(`document.documentElement.outerHTML || ""`, 10*time.Second)
	if err != nil || html == "" {
		if e.logf != nil {
			e.logf(fmt.Sprintf("[DumpPageHTML] ExecJSResult 读回 HTML 失败：%v", err))
		}
		return ""
	}
	_ = os.MkdirAll(e.diagDir, 0o755)
	name := fmt.Sprintf("page_%s_%s.html", tag, time.Now().Format("20060102_150405"))
	path := filepath.Join(e.diagDir, name)
	if err := os.WriteFile(path, []byte(html), 0o644); err != nil {
		return ""
	}
	return path
}
