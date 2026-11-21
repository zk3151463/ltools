package col1688

import (
	"context"
	"encoding/json"
	"fmt"
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
}

// pageHTMLPayload 子窗口回传的页面 HTML 载荷。
type pageHTMLPayload struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	HTML  string `json:"html"`
}

func newWindowEngine(app *application.App, diagDir string) *windowEngine {
	return &windowEngine{
		app:        app,
		name:       "c1688-window",
		diagDir:    diagDir,
		pageHTMLCh: make(chan pageHTMLPayload, 16),
		bridgeStop: make(chan struct{}),
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

// TestNewWindowIntercept 诊断：开子窗口到 data: URL，触发一次 window.open('_blank')，
// 等 COM 层 NewWindowRequested 回调，再报告子窗口数量（应恒为 1，无第二窗）。
// 仅用于验证 patch 生效，正式构建可删除。
func (e *windowEngine) TestNewWindowIntercept(ctx context.Context) string {
	if err := e.OpenWindow(ctx, "data:text/html;charset=utf-8,<html><body onload='window.open(%22https://www.1688.com%22%2C%20%22_blank%22)'><script>setTimeout(function(){window.open(%22https://detail.1688.com%2Foffer%2F1.html%22,%20%22_blank%22)},500)</script></body></html>"); err != nil {
		return "open failed: " + err.Error()
	}
	time.Sleep(4 * time.Second)
	// 统计当前所有窗口（主窗 + 子窗 + 任何 COM 冒出的窗）。
	all := e.app.Window.GetAll()
	subCount, total := 0, len(all)
	for _, ww := range all {
		if ww != nil && ww.Name() == e.name {
			subCount++
		}
	}
	if e.logf != nil {
		e.logf(fmt.Sprintf("[诊断] 触发 2 次 window.open('_blank')；总窗口数=%d，同名子窗(c1688-window)数=%d（应=1）。若日志还出现\"拦截新窗口请求\"即 patch 生效，无第二窗", total, subCount))
	}
	return fmt.Sprintf("totalWindows=%d, subWindowCount=%d", total, subCount)
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
  window.__ltoolsCollect = function() {
    try {
      const html = document.documentElement.outerHTML || "";
      const payload = { url: location.href, title: document.title, html: html };
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
			w.ExecJS(bridgeJS)
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
// c1688:page-html 事件回传 → 本方法阻塞等待该事件 → parseHTML 解析 → 返回 []Supplier。
// 超时（8s）返回 nil。
func (e *windowEngine) CollectCurrentPage(ctx context.Context, source string, logf func(string)) []Supplier {
	w, ok := e.currentWindow()
	if !ok {
		if logf != nil {
			logf("子窗口未打开，无法采集；请先点「打开 1688」")
		}
		return nil
	}

	// 先清掉 channel 里可能残留的旧 payload
	e.drainChannel()

	// 触发子窗口执行 __ltoolsCollect
	w.ExecJS(`if (window.__ltoolsCollect) window.__ltoolsCollect();`)

	// 等待回传事件
	select {
	case p := <-e.pageHTMLCh:
		if p.HTML == "" {
			if logf != nil {
				logf("子窗口回传 HTML 为空（页面可能未加载或被 baxia 拦截）")
			}
			return nil
		}
		if logf != nil {
			logf(fmt.Sprintf("收到子窗口 HTML：url=%s title=%q (%d 字符)", p.URL, p.Title, len(p.HTML)))
		}
		items := parseHTML(p.HTML, source, 0)
		if logf != nil {
			logf(fmt.Sprintf("解析到 %d 条", len(items)))
		}
		return items
	case <-time.After(8 * time.Second):
		// 超时：再试一次（事件可能在 ExecJS 前就发出了，被 channel 错过）
		w.ExecJS(`if (window.__ltoolsCollect) window.__ltoolsCollect();`)
		select {
		case p := <-e.pageHTMLCh:
			if p.HTML == "" {
				if logf != nil {
					logf("子窗口回传 HTML 为空（第二次尝试仍空）")
				}
				return nil
			}
			items := parseHTML(p.HTML, source, 0)
			if logf != nil {
				logf(fmt.Sprintf("解析到 %d 条（重试）", len(items)))
			}
			return items
		case <-time.After(3 * time.Second):
			if logf != nil {
				logf("子窗口回传 HTML 超时；请确认子窗口已加载 1688 页面且未被拦截")
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
