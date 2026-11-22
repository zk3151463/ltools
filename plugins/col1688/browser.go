package col1688

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/edge"
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

	// onCreatedRegistered 防 registerWindowGuard 重复注册 OnCreate。
	onCreatedRegistered bool
	// logf 日志钩子，由 service 注入（engine 自身不持有 store/app 日志通道）。
	// 允许为 nil（OnCreate 回调里做 nil 检查）。
	logf func(string)

	// lastPageHTML 缓存子窗口 collectJS 自动回传的最近一次结构化数据（c1688:collect）。
	// 由 handleRawMessage 更新。CollectCurrentPage 纯读此缓存（不等待、不触发、不 HTTP 回退）。
	lastPageMu   sync.Mutex
	lastPageHTML pageHTMLPayload
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
// htmlServerOwned 参数保留以兼容调用方，但 HTTP 回传已废弃，不再启动本地服务。
func newWindowEngine(app *application.App, diagDir string, htmlServerOwned bool) *windowEngine {
	e := &windowEngine{
		app:        app,
		name:       "c1688-window",
		diagDir:    diagDir,
		pageHTMLCh: make(chan pageHTMLPayload, 16),
	}
	_ = htmlServerOwned // 已废弃：collectJS 走 window.external.invoke 官方通道，无需本地 HTTP 服务
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

// rawMessageListeners 包级 channel：main.go 的 application.Options.RawMessageHandler
// 把子窗口 window.external.invoke 的原始 JSON 消息写进来，各 engine 的 handleRawMessage
// 监听消费。用 slice + mutex 支持多个 engine（列表 + 详情）。
var (
	rawMsgMu       sync.Mutex
	rawMsgHandlers []func(message string)
)

// SetRawMessageHandler 由 main.go 在创建 app 时调用（RawMessageHandler 闭包内）。
// 注册一个接收 window.external.invoke 原始消息的回调。
func SetRawMessageHandler(fn func(message string)) {
	rawMsgMu.Lock()
	rawMsgHandlers = append(rawMsgHandlers, fn)
	rawMsgMu.Unlock()
}

// dispatchRawMessage 由 main.go 的 RawMessageHandler 调用，把 message 分发给所有已注册的 handler。
func DispatchRawMessage(message string) {
	// 诊断：确认 DispatchRawMessage 被调用（log.Printf 走标准输出，task dev 可见）
	log.Printf("[col1688] DispatchRawMessage called（len=%d）", len(message))
	rawMsgMu.Lock()
	handlers := make([]func(string), len(rawMsgHandlers))
	copy(handlers, rawMsgHandlers)
	rawMsgMu.Unlock()
	if len(handlers) == 0 {
		log.Printf("[col1688] DispatchRawMessage: no handlers registered!")
		return
	}
	for _, h := range handlers {
		h(message)
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
	// 注册 window.external.invoke 消息处理器（子窗口 __ltoolsCollect 发 c1688:page-html
	// JSON payload，Wails 框架层 RawMessageHandler → DispatchRawMessage → 这里）。
	SetRawMessageHandler(func(message string) {
		e.handleRawMessage(message)
	})
	log.Printf("[col1688] registerEvents: raw message handler registered（engine=%s）", e.name)
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

// handleRawMessage 处理子窗口 window.external.invoke 发来的原始 JSON 消息。
// 消息格式：JSON.stringify({ url, title, html, items })——与 pageHTMLPayload 一致。
// 用 "c1688" 或 JSON 对象特征过滤，非本插件的消息直接丢弃。
func (e *windowEngine) handleRawMessage(message string) {
	// 诊断：无条件打日志（确认 RawMessageHandler 是否被调用、消息内容是什么）
	if e.logf != nil {
		e.logf(fmt.Sprintf("[RawMsg] 收到 raw message（len=%d）：%s", len(message), truncateStr(message, 200)))
	}
	// 快速过滤：消息必须是 JSON 对象
	if !strings.HasPrefix(message, "{") {
		return
	}
	// 先解析 type 字段（区分 c1688:connected 和 page-html payload）
	var msg struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(message), &msg); err != nil {
		if e.logf != nil {
			e.logf(fmt.Sprintf("[RawMsg] JSON 解析失败：%v（raw=%s）", err, truncateStr(message, 100)))
		}
		return
	}
	// c1688:connected：子窗口页面加载后发来的连接确认
	if msg.Type == "c1688:connected" {
		var c struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal([]byte(message), &c)
		if e.logf != nil {
			e.logf(fmt.Sprintf("[连接] 子窗口已连接 via window.external.invoke（url=%s）", c.URL))
		}
		return
	}
	// c1688:collect：collectJS 自动回传的结构化数据（items 已整理为 JSON 数组）
	if msg.Type == "c1688:collect" {
		var p pageHTMLPayload
		if err := json.Unmarshal([]byte(message), &p); err != nil {
			if e.logf != nil {
				e.logf(fmt.Sprintf("[RawMsg] c1688:collect 解析失败：%v（raw=%s）", err, truncateStr(message, 100)))
			}
			return
		}
		if len(p.Items) == 0 {
			if e.logf != nil {
				e.logf("[RawMsg] c1688:collect 0 条（数据未就绪或非结果页），丢弃")
			}
			return
		}
		if e.logf != nil {
			e.logf(fmt.Sprintf("[回传] 收到 %d 条供应商（url=%s）via window.external.invoke", len(p.Items), p.URL))
		}
		// 写缓存：CollectCurrentPage 直接读此缓存（不等待、不触发、不 HTTP 回退）
		e.lastPageMu.Lock()
		e.lastPageHTML = p
		e.lastPageMu.Unlock()
		select {
		case e.pageHTMLCh <- p:
		default:
		}
		return
	}
	// 其他 type / 无 type → 非本插件消息，丢弃
	if e.logf != nil {
		e.logf(fmt.Sprintf("[RawMsg] 忽略未知 type=%s", msg.Type))
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
	e.lastPageMu.Lock()
	defer e.lastPageMu.Unlock()
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
	// 注入两个独立跨导航脚本（各 <12KB，AddScript 各调一次，避免 WebView2 单脚本超限静默丢弃）：
	//   collectJS  = 取数（window.data.offerV2 精确路径）+ 自动回传（window.external.invoke）
	//   navGuardJS = 导航拦截 + 浅色渲染
	// 跨导航注入（AddScriptToExecuteOnDocumentCreated，每个新文档 JS 运行前都执行），
	// 不经过 ExecJS COM 调用（不碰 RESOURCE_NOT_IN_CORRECT_STATE）。
	edge.SetExtraInitScripts(collectJS, navGuardJS)
	log.Printf("[col1688] OpenWindow: ExtraInitScripts set（collectJS=%d, navGuardJS=%d）", len(collectJS), len(navGuardJS))
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
	// collectJS + navGuardJS 经 AddScriptToExecuteOnDocumentCreated 跨导航自动注入，无需 install 循环重注。
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

// OpenLogin 打开子窗口到 1688 首页（供用户登录）。
func (e *windowEngine) OpenLogin() error {
	return e.OpenWindow(context.Background(), "https://www.1688.com/")
}

// CloseLogin 关闭子窗口。
func (e *windowEngine) CloseLogin() {
	e.CloseWindow()
}

// collectJS 注入 1688 子窗口的跨导航脚本 A（AddScriptToExecuteOnDocumentCreated，每个新文档
// JS 运行前执行）。只做取数 + 回传（导航拦截 + 浅色渲染在独立的 navGuardJS，两脚本各 <12KB，
// 避免 WebView2 单脚本超限静默丢弃）：
//
//  1. 取数：1688 搜索结果页（s.1688.com/selloffer/offer_search.htm）数据在
//     window.data.offerV2.response.data.OFFER.items[]，每条映射：
//       - name     取 item.data.shop.text（店铺名）
//       - url      取 item.data.shopAddition.shopLinkUrl（店铺链接）
//       - bizType  取 item.data.bizType（店铺类型，放 level 字段透传）
//     电话/地址搜索结果页没有，留空，由 Go 端详情页双窗口编排补齐。
//     非搜索页（首页/登录页）读不到该路径 → 回传空数组。
//  2. 自动回传：等 data 就绪（最多 10s，1s 间隔）→ window.external.invoke(JSON.stringify(
//     {type:'c1688:collect', items, url, title})) 经 Wails RawMessageHandler 回传 Go 端。
//
// 幂等：window.__ltoolsCollectInstalled 标记；c1688:connected 握手确认通道已通。
const collectJS = `
(function(){
  // ---- 幂等：本文档已装则直接返回（AddScript 每个新文档都执行，新文档上下文重置标记）----
  if (window.__ltoolsCollectInstalled) return;
  window.__ltoolsCollectInstalled = true;

  // ---- c1688:connected 握手（确认 window.external.invoke 通道已通）----
  if (!window.__ltoolsConnected && window.external && window.external.invoke) {
    window.__ltoolsConnected = true;
    try {
      window.external.invoke(JSON.stringify({
        type: 'c1688:connected',
        url: location.href,
        ts: Date.now()
      }));
    } catch (e) {}
  }

  // ---- 取数工具 ----
  function normName(s){ if(!s) return ""; s=String(s).trim(); if(s.length<2||s.length>80) return ""; return s; }

  // ---- 1688 搜索结果页取数（精确路径，唯一策略）----
  function collect(){
    var list=[];
    var items=null;
    try{
      if (window.data && window.data.offerV2 && window.data.offerV2.response &&
          window.data.offerV2.response.data && window.data.offerV2.response.data.OFFER &&
          window.data.offerV2.response.data.OFFER.items) {
        items = window.data.offerV2.response.data.OFFER.items;
      }
    }catch(e){}
    if (items && items.length){
      items.forEach(function(it){
        try{
          var d = it.data || it;
          var shop = (d.shop && typeof d.shop === "object") ? d.shop : {};
          var addition = (d.shopAddition && typeof d.shopAddition === "object") ? d.shopAddition : {};
          var name = normName(shop.text || shop.name || shop.companyName || "");
          var url = addition.shopLinkUrl || addition.url || "";
          var bizType = String(d.bizType || it.bizType || "");
          if(name || url){
            list.push({name:name, phone:"", address:"", main:"", level:bizType, url:url});
          }
        }catch(e){}
      });
    }
    window.__ltoolsCollectItems = list;
    window.__ltoolsCollectItemsJSON = JSON.stringify(list||[]);
    return list;
  }
  // 文档创建时立即跑一次（此时 1688 数据多半没到，结果空；后续自动回传轮询会重取）
  collect();
  window.__ltoolsCollectItemsRefresh = collect;

  // ---- 自动回传：等数据就绪再取 + invoke（不靠 Go 端 ExecJS 触发）----
  // 1688 的 window.data / DOM 在 document.readyState=complete 后才稳定。
  // 轮询 window.data 存在或 readyState complete，最多 10s，1s 间隔。
  // 同一文档只发一次（window.__ltoolsCollectSent），整页导航后新文档上下文重置标记。
  var sent = false;
  var timer = setInterval(function(){
    if (sent) { clearInterval(timer); return; }
    var dataReady = (typeof window.data === "object" && window.data) ||
                    document.readyState === "complete";
    if (!dataReady) {
      if (window.__ltoolsCollectPolls == null) window.__ltoolsCollectPolls = 0;
      window.__ltoolsCollectPolls++;
      if (window.__ltoolsCollectPolls > 10) { clearInterval(timer); }
      return;
    }
    sent = true;
    window.__ltoolsCollectSent = true;
    clearInterval(timer);
    var items = collect();
    var payload = {
      type: 'c1688:collect',
      items: items,
      url: location.href,
      title: document.title,
      ts: Date.now()
    };
    if (window.external && window.external.invoke) {
      try { window.external.invoke(JSON.stringify(payload)); } catch(e){}
    }
  }, 1000);
})();`

// navGuardJS 注入 1688 子窗口的第二个跨导航脚本（与 collectJS 独立，AddScript 各调一次，
// 单脚本体积 < 12KB 避免 WebView2 静默丢弃）。做两件事：
//
//  1. 导航拦截（click/submit/window.open/原型劫持三层全拦）：把“会开新窗口/跳出 1688”的
//     意图全部改成当前窗口内跳转，降低 WebView2 COM 层 NewWindowRequested 触发频率
//     （真正的兜底关窗靠 Go 端 OnCreate）。
//  2. 浅色渲染（:root color-scheme=light + meta + 兜底 css）：抑制 WebView2 对未声明
//     color-scheme 页面的 auto-darken（“1688 首页偏暗”根因）。
//
// 幂等：window.__ltoolsNavInstalled 标记，同一文档内不重复装；与 collectJS 幂等独立。
const navGuardJS = `
(function(){
  if (window.__ltoolsNavInstalled) return;
  window.__ltoolsNavInstalled = true;

  // ---- 导航拦截（事件 + 原型 + window.open 三层全拦）----

  const is1688 = function(u){
    if (!u) return true;
    try { return /(^|\\.)1688\\.com$/i.test(new URL(u, location.href).hostname); } catch(e){ return true; }
  };
  const willOpenNew = function(target, href){
    const t = (target || '').toLowerCase();
    if (t === '_blank' || t === '_parent' || t === '_top') return true;
    if (href && !is1688(href) && /^https?:/i.test(href)) return true;
    return false;
  };
  const fakeWin = function(){
    return { close:function(){}, focus:function(){}, closed:false, location:{ href:"" }, document:{ write:function(){}, close:function(){} } };
  };
  const openHook = function(url, name, features){
    if (typeof url === 'string' && url.length && url !== 'about:blank' && !/^about:/i.test(url)) {
      try { window.location.href = url; } catch(e) {}
      return fakeWin();
    }
    return fakeWin();
  };
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

  // ---- 浅色渲染（抑制 WebView2 auto-darken）----
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


// truncateStr 截断长字符串（日志用）。
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// CollectCurrentPage 读取子窗口当前页缓存（collectJS 自动回传的结构化数据），返回 []Supplier。
//
// 简化链路（官方通道唯一）：collectJS 在 1688 子窗口自动取数 + window.external.invoke
// 回传（c1688:collect），handleRawMessage 收后写 lastPageHTML 缓存。本方法纯读缓存：
// 命中直接返回；缓存空说明 collectJS 尚未回传（1688 页未加载完 / 非结果页），提示用户稍候。
// 不等待、不 ExecJS 触发、不 HTTP 回退——等待策略后续单独重写。
func (e *windowEngine) CollectCurrentPage(ctx context.Context, source string, logf func(string)) []Supplier {
	w, ok := e.currentWindow()
	_ = w
	if !ok {
		if logf != nil {
			logf("子窗口未打开，无法采集；请先点「打开 1688」")
		}
		return nil
	}
	e.lastPageMu.Lock()
	p := e.lastPageHTML
	e.lastPageMu.Unlock()
	if len(p.Items) > 0 {
		for i := range p.Items {
			if p.Items[i].Source == "" {
				p.Items[i].Source = source
			}
			if p.Items[i].Time == "" {
				p.Items[i].Time = nowLocal()
			}
		}
		if logf != nil {
			logf(fmt.Sprintf("[缓存] 命中自动回传（%d 条，url=%s）", len(p.Items), p.URL))
		}
		return p.Items
	}
	if logf != nil {
		logf("缓存为空（collectJS 尚未回传或当前页无供应商）；请确认 1688 搜索页已加载完成，稍候再点「采集」")
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


// CollectAllPages 连续多页采集：当前页 + 用户手动翻页 N 次。
// 翻页由用户在子窗口里手动点「下一页」（拟人轨迹最真实），本方法只读当前页一次。
// 多页采集由前端循环调用 CollectCurrentPage 完成（用户每次手动翻页后再点一次）。
func (e *windowEngine) CollectAllPages(ctx context.Context, o CollectOptions, logf func(string)) []Supplier {
	return e.CollectCurrentPage(ctx, o.Keyword, logf)
}

// CloseWindow 关闭子窗口（按名字全局查，跨实例安全）。
func (e *windowEngine) CloseWindow() {
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






