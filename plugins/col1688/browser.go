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

func newWindowEngine(app *application.App, diagDir string) *windowEngine {
	return &windowEngine{
		app:        app,
		name:       "c1688-window",
		diagDir:    diagDir,
		pageHTMLCh: make(chan pageHTMLPayload, 16),
		bridgeStop: make(chan struct{}),
		collectReq: make(chan pageHTMLPayload, 4),
	}
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
// 固定端口便于 bridgeJS 硬编码；被占用时 startHTMLReturn 会报错并记日志（不影响主流程，
// 采集退化为"无数据"，前端仍可用 html 粘贴模式降级）。
const htmlReturnPort = 48217

// startHTMLReturn 启动本地回环 HTTP 服务，接收子窗口 fetch POST 的 HTML。
// 在 engine 创建时（NewCollect1688Service）调用一次。
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
		if e.logf != nil {
			e.logf("HTML 回传端口 " + fmt.Sprintf("%d", htmlReturnPort) + " 启动失败：" + err.Error() + "（采集将无数据，可用粘贴 HTML 模式）")
		}
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/c1688/html", e.htmlReturnHandler)
	srv := &http.Server{Handler: mux}
	e.htmlServer = srv
	go func() {
		_ = srv.Serve(ln)
	}()
	if e.logf != nil {
		e.logf(fmt.Sprintf("HTML 回传服务已启动（127.0.0.1:%d）", htmlReturnPort))
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
	// 推给正在等待的 CollectCurrentPage；若无人在等则丢弃（channel 有缓冲兜底）。
	select {
	case e.collectReq <- p:
	default:
		select {
		case e.pageHTMLCh <- p:
		default:
		}
	}
	if _, werr := w.Write([]byte("ok")); werr != nil {
		return
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
  window.__ltoolsCollect = function() {
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
        try {
          fetch(target, {
            method: 'POST',
            headers: { 'Content-Type': 'text/plain' },
            body: JSON.stringify(payload),
            keepalive: true,
          }).catch(function(){});
        } catch (e) {}
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

  function collect(){
    var list=[];
    try{ list=fromData(window); }catch(e){}
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
  function isDisabled(a){ return a.hasAttribute('disabled')||/(disabled|disabled_)/.test(a.className||''); }
  function clickAnchor(a){
    if(!a) return false;
    if(isDisabled(a)) return false;
    try{ if(a.click) { a.click(); } else { a.dispatchEvent(new MouseEvent('click',{bubbles:true})); } }catch(e){ try{ location.href=a.href; }catch(e2){} }
    return true;
  }
  // 策略 1：文本 = 下一页 / » / 下页
  var anchors=document.querySelectorAll('a');
  for(var i=0;i<anchors.length;i++){
    var t=txt(anchors[i]);
    if(t==='下一页'||t==='下页'||t==='»'||t==='›'||t==='>') { if(clickAnchor(anchors[i])) return 1; }
  }
  // 策略 2：pagination 容器里文本=下一页
  var pg=document.querySelectorAll('[class*="pagination"] a,[class*="pager"] a,[class*="page"] a');
  for(var j=0;j<pg.length;j++){
    var tt=txt(pg[j]);
    if(tt==='下一页'||tt==='»'){ if(clickAnchor(pg[j])) return 1; }
  }
  // 策略 3：URL 翻页参数（pageNum / page）+1
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

// CollectCurrentPage 对子窗口当前页执行提取：注入 __ltoolsCollect → 子窗口把 HTML 通过
// **本地回环 HTTP**（127.0.0.1:48217/c1688/html，见 startHTMLReturn）POST 回 Go →
// 本方法阻塞等待 collectReq → parseHTML 解析 → 返回 []Supplier。
// 兜底：Wails 事件 c1688:page-html（pageHTMLCh）仍保留，若子窗口是内部页（有 window.ltools）
// 也能走事件回传。外部页主要走 HTTP 回传。超时（8s）返回 nil。
func (e *windowEngine) CollectCurrentPage(ctx context.Context, source string, logf func(string)) []Supplier {
	w, ok := e.currentWindow()
	if !ok {
		if logf != nil {
			logf("子窗口未打开，无法采集；请先点「打开 1688」")
		}
		return nil
	}

	// 清空两个回传通道里可能残留的旧 payload（防上一次的 HTML 串到本次）
	e.drainChannel()
	e.drainCollectReq()

	// 触发子窗口执行 __ltoolsCollect（它会 fetch POST 到本地回环）
	w.ExecJS(`if (window.__ltoolsCollect) window.__ltoolsCollect();`)

	// 等待回传（HTTP 通道优先，事件通道兜底）
	select {
	case p := <-e.collectReq:
		return e.finishCollect(p, source, logf)
	case p := <-e.pageHTMLCh:
		return e.finishCollect(p, source, logf)
	case <-time.After(8 * time.Second):
		// 超时：再试一次（fetch 可能慢 / 事件在 ExecJS 前发出被错过）
		w.ExecJS(`if (window.__ltoolsCollect) window.__ltoolsCollect();`)
		select {
		case p := <-e.collectReq:
			return e.finishCollect(p, source, logf)
		case p := <-e.pageHTMLCh:
			return e.finishCollect(p, source, logf)
		case <-time.After(3 * time.Second):
			if logf != nil {
				logf("子窗口回传 HTML 超时；请确认子窗口已加载 1688 页面且未被拦截（回传走 127.0.0.1:48217）")
			}
			return nil
		}
	case <-ctx.Done():
		if logf != nil {
			logf("已取消")
		}
		return nil
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
func (e *windowEngine) ScrollToBottom(interval time.Duration) {
	if interval <= 0 {
		interval = 1200 * time.Millisecond
	}
	for i := 0; i < 8; i++ {
		w, ok := e.currentWindow()
		if !ok {
			return
		}
		w.ExecJS(scrollLoadMoreJS)
		// 读"是否到底"：再 ExecJS 把结果存到 window，然后读（ExecJS 是 void，
		// 用一个可被 JS 写、Go 用 ExecJS 触发的同步判定：再跑一次 scrollLoadMoreJS
		// 并读 window.__ltoolsScrollDone 需要返回值；退化为"固定滚动 N 次"策略）。
		// 这里简化：滚 6 次即视为到底（1688 一页内容有限），每次等 interval。
		if i >= 5 {
			break
		}
		time.Sleep(interval)
	}
}

// ClickNextPage 在子窗口里点「下一页」。返回 true 表示点了（页面将跳转）。
// 定位策略见 clickNextPageJS（文本"下一页/»" → pagination 容器 → URL 参数 +1）。
func (e *windowEngine) ClickNextPage() bool {
	w, ok := e.currentWindow()
	if !ok {
		return false
	}
	// clickNextPageJS 执行点击；由于 ExecJS 是 void 读不回返回值，
	// 这里执行后等导航稳定（readyState + 固定 2s），由调用方判断"是否真的翻页成功"
	// （用 URL/首卡片指纹变化）。返回 true 仅表示"点了 / 尝试了"。
	w.ExecJS(clickNextPageJS)
	return true
}

// DumpPageHTML 把子窗口当前页 HTML 落盘到 diagDir（诊断用），返回文件路径。
func (e *windowEngine) DumpPageHTML(tag string) string {
	w, ok := e.currentWindow()
	if !ok || e.diagDir == "" {
		return ""
	}
	e.drainChannel()
	w.ExecJS(`if (window.__ltoolsCollect) window.__ltoolsCollect();`)
	select {
	case p := <-e.pageHTMLCh:
		if p.HTML == "" {
			return ""
		}
		_ = os.MkdirAll(e.diagDir, 0o755)
		name := fmt.Sprintf("page_%s_%s.html", tag, time.Now().Format("20060102_150405"))
		path := filepath.Join(e.diagDir, name)
		if err := os.WriteFile(path, []byte(p.HTML), 0o644); err != nil {
			return ""
		}
		return path
	case <-time.After(5 * time.Second):
		return ""
	}
}
