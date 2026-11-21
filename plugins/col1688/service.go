package col1688

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// Collect1688Service 暴露给前端 Wails 的服务。
//
// 前端用法（参考 dryygjx 控制条）：
//   - OpenLogin()          登录浏览器（起 Chrome 供登录 1688）
//   - Start(opts)          开始采集
//   - Pause() / Resume()   暂停/恢复
//   - Stop()               停止
//   - ParseHTML(html)      粘贴 HTML 解析（离线主用）
//   - GetResults()         取当前结果
//   - ExportCSV()          导出为 CSV，返回文件路径
//   - GetLogs()            取运行日志
//
// 运行态事件：
//   c1688:log      {time, msg}
//   c1688:result   []Supplier
//   c1688:progress {total}
//   c1688:state    "idle"|"running"|"paused"|"stopped"
type Collect1688Service struct {
	app      *application.App
	engine   *windowEngine
	detailEng *windowEngine // 详情页子窗口引擎（独立窗口，补电话用；风控上可单独关窗重置）
	store    *store
	dataDir  string

	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	running bool
	paused  bool
	wg      sync.WaitGroup

	results []Supplier
	logs    []LogEntry
	state   RunState
}

// NewCollect1688Service 创建服务。dataDir 为插件数据目录。
// 同时创建列表引擎（起 127.0.0.1:48217 回传服务）和详情引擎（复用同一台回传服务，独立窗口）。
func NewCollect1688Service(app *application.App, dataDir string) *Collect1688Service {
	ctx, cancel := context.WithCancel(context.Background())
	svc := &Collect1688Service{
		app:       app,
		engine:    newWindowEngine(app, filepath.Join(dataDir, "diag"), true),
		detailEng: newDetailWindowEngine(app, filepath.Join(dataDir, "diag")),
		store:     NewStore(dataDir),
		dataDir:   dataDir,
		ctx:       ctx,
		cancel:    cancel,
		state:     StateIdle,
	}
	// 注入日志钩子，供 OnCreate 兜底钩子（关 WebView2 冒出的新窗）写日志
	svc.engine.SetLogf(svc.logf)
	svc.detailEng.SetLogf(svc.logf)
	// 注册子窗口回传 HTML 的事件监听 + 窗口创建兜底（必须在任何 OpenWindow 之前）
	svc.engine.registerEvents()
	svc.detailEng.registerEvents()
	// 启动子窗口 HTML 回传的本地回环 HTTP 服务（外部页 fetch POST 回来；必须在 OpenWindow 前）。
	// 列表引擎已在 newWindowEngine(owned=true) 里起过，详情引擎 startHTMLReturn 幂等跳过。
	svc.engine.startHTMLReturn()
	svc.detailEng.startHTMLReturn()
	return svc
}

// logf 记录日志并推事件。
func (s *Collect1688Service) logf(msg string) {
	s.mu.Lock()
	entry := LogEntry{Time: nowLocal(), Msg: msg}
	s.logs = append(s.logs, entry)
	if len(s.logs) > 2000 {
		s.logs = s.logs[len(s.logs)-2000:]
	}
	s.mu.Unlock()
	if s.app != nil {
		_ = s.app.Event.Emit("c1688:log", entry)
	}
}

// getState 返回当前状态字符串。
func (s *Collect1688Service) getState() string {
	switch s.state {
	case StateRunning:
		return "running"
	case StatePaused:
		return "paused"
	case StateStopped:
		return "stopped"
	default:
		return "idle"
	}
}

func (s *Collect1688Service) emitState() {
	if s.app != nil {
		_ = s.app.Event.Emit("c1688:state", s.getState())
	}
}

func (s *Collect1688Service) emitProgress() {
	s.mu.Lock()
	total := len(s.results)
	s.mu.Unlock()
	if s.app != nil {
		_ = s.app.Event.Emit("c1688:progress", map[string]int{"total": total})
	}
}

// Start 开始采集（browser 或 html 模式）。
func (s *Collect1688Service) Start(opts CollectOptions) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return errors.New("已有采集任务在运行")
	}
	s.running = true
	s.paused = false
	s.state = StateRunning
	s.results = nil
	// 重建可取消上下文，使每次 Start 都可被 Stop 取消
	s.ctx, s.cancel = context.WithCancel(context.Background())
	ctx := s.ctx
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			s.running = false
			s.state = StateStopped
			s.mu.Unlock()
			s.emitState()
		}()

		switch opts.Mode {
		case "html":
			s.collectFromHTML(opts)
		default: // browser
			s.collectFromBrowser(opts, ctx)
		}
	}()
	s.emitState()
	return nil
}

// collectFromHTML 粘贴 HTML 解析。
func (s *Collect1688Service) collectFromHTML(opts CollectOptions) {
	src := "粘贴HTML"
	if opts.Keyword != "" {
		src = "粘贴HTML " + opts.Keyword
	}
	items := parseHTML(opts.HTML, src, opts.MaxCount)
	s.mu.Lock()
	s.results = items
	s.mu.Unlock()
	s.logf(fmt.Sprintf("解析完成，共 %d 条有效记录", len(items)))
	s.store.Save(opts.Keyword, items)
	if s.app != nil {
		_ = s.app.Event.Emit("c1688:result", items)
	}
	s.emitProgress()
}

// collectFromBrowser Wails 子窗口采集（用户手动操作 + JS 注入读当前页）。
func (s *Collect1688Service) collectFromBrowser(opts CollectOptions, ctx context.Context) {
	s.logf("开始子窗口采集（请在 1688 子窗口里手动完成登录/搜索/调地区，再点「采集当前页」）")
	if s.engine.IsWindowOpen() {
		s.logf("子窗口已打开，等待用户在子窗口里操作")
	} else {
		s.logf("⚠️ 子窗口未打开；已自动打开子窗口到搜索页")
		_ = s.engine.OpenWindow(ctx, s.engine.searchURL(opts))
	}
	// 阻塞等待用户手动触发 CollectCurrentPage（由前端按钮触发）。
	// 这里不做轮询；前端点「采集当前页」会单独调用 CollectCurrentPage 方法。
	s.logf("等待用户在子窗口里完成操作后，点「采集当前页」开始读取数据")
}

// CollectCurrentPage 读取子窗口当前页并解析供应商数据（用户手动在子窗口完成搜索/登录后触发）。
// 返回解析出的供应商记录（可能为空），并把结果累积到 s.results。
func (s *Collect1688Service) CollectCurrentPage() []Supplier {
	if !s.engine.IsWindowOpen() {
		s.logf("子窗口未打开，无法采集；请先点「打开 1688」")
		return nil
	}
	// 用当前子窗口 URL 作为 source（供结果表展示来源）
	src := "子窗口当前页"
	items := s.engine.CollectCurrentPage(context.Background(), src, func(msg string) {
		s.logf(msg)
	})
	if items == nil {
		return nil
	}
	// 累积到结果（去重）
	s.mu.Lock()
	s.results = dedup(append(s.results, items...))
	all := make([]Supplier, len(s.results))
	copy(all, s.results)
	s.mu.Unlock()
	s.store.Save("", all)
	_ = s.app.Event.Emit("c1688:result", all)
	s.emitProgress()
	return all
}

// OpenLogin 打开 1688 子窗口供用户登录（不立即采集）。
func (s *Collect1688Service) OpenLogin() error {
	if err := s.engine.OpenLogin(); err != nil {
		s.logf(err.Error())
		return err
	}
	s.logf("子窗口已打开 1688，请完成登录")
	return nil
}

// OpenSearch 打开（或导航已开的）1688 子窗口到"关键词 + 类别"搜索结果页。
//
// 方案 C：由 Go 代搜——把关键词/类别拼成 1688 站内搜索 URL 直接 SetURL 到子窗口，
// 避免用户手敲搜索框触发跨域表单提交（login.1688.com）→ WebView2 COM 层冒新窗口。
// keyword 为空时到 1688 首页（供先登录）。category：factory/supplier/industry/offer。
func (s *Collect1688Service) OpenSearch(keyword, category string) error {
	err := s.engine.OpenSearch(context.Background(), keyword, category)
	if err != nil {
		s.logf("打开搜索页失败：" + err.Error())
		return err
	}
	if keyword != "" {
		s.logf(fmt.Sprintf("已打开搜索页：%s（%s）", keyword, categoryOrDefault(category)))
	} else {
		s.logf("子窗口已打开 1688 首页（未带关键词，可先登录）")
	}
	return nil
}

// categoryOrDefault 返回类别的中文标签（日志用），未知回退 offer。
func categoryOrDefault(c string) string {
	switch c {
	case "factory":
		return "找工厂"
	case "supplier":
		return "找供应商"
	case "industry":
		return "行业"
	default:
		return "找货源"
	}
}

// CloseLogin 关闭 1688 子窗口。
func (s *Collect1688Service) CloseLogin() {
	s.engine.CloseLogin()
	s.logf("已关闭 1688 子窗口")
}

// DumpDiagnosis 把子窗口当前页 HTML 落盘到 diagDir（诊断用），返回文件路径。
// 子窗口未开时返回明确提示，不报错。
func (s *Collect1688Service) DumpDiagnosis() (string, error) {
	if !s.engine.IsWindowOpen() {
		return "", errors.New("1688 子窗口未打开，无法生成诊断；请先点「打开 1688」")
	}
	path := s.engine.DumpPageHTML("manual")
	if path == "" {
		return "", errors.New("未能取到子窗口当前页 HTML（页面可能未加载或被拦截）")
	}
	s.logf("诊断 HTML 已保存：" + path)
	return path, nil
}

// Pause 暂停。
func (s *Collect1688Service) Pause() {
	s.mu.Lock()
	if s.running {
		s.paused = true
		s.state = StatePaused
	}
	s.mu.Unlock()
	s.emitState()
}

// Resume 恢复。
func (s *Collect1688Service) Resume() {
	s.mu.Lock()
	if s.running {
		s.paused = false
		s.state = StateRunning
	}
	s.mu.Unlock()
	s.emitState()
}

// Stop 停止（取消后台 goroutine）。
func (s *Collect1688Service) Stop() {
	s.mu.Lock()
	if s.running {
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
	s.logf("已停止")
}

// StartPages 自动多页采集（Go 端编排）：
// 用户在子窗口里先手动搜到结果页（登录/关键词/类别），然后点本按钮，
// 程序自动：滚到底 → 取当前页数据 → 点下一页 → 翻页间隔 DelaySec → 重复，共 Pages 页。
// 全程在子窗口内（COM 拦截保证不冒新窗）。累积去重结果，每页推 c1688:progress。
//
// opts.Pages 0 或缺省 → 3 页；opts.DelaySec 0 → 2s；opts.MaxCount 0 → 不限。
// 若某页"卡住"（连续 2 页无新增数据，说明翻页没成功 / 到底了）→ 提前停。
// 返回 nil（结果走 c1688:result 事件 + GetResults）。
//
// 双窗编排（可选，opts.DetailEnabled=true）：
//   - 每页采完列表数据后，对 item.Url（去重）最多 opts.DetailPerPage 条（默认 5、上限 10）
//     在 detailEngine（独立子窗口）里 SetURL 逐个访问，用 collectFromDetailWindow 补电话。
//   - 节奏：详情间隔 800ms + 0~400ms 抖动（模拟浏览，不固定节拍）；详情被拒（取不到）→
//     日志提示后自动停当轮详情循环，列表采集继续。
//   - 详情窗口首登：首次使用请先点「打开详情窗（首登）」登录 1688（登录态持久在 detail 窗口）。
func (s *Collect1688Service) StartPages(opts CollectOptions) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return errors.New("已有采集任务在运行")
	}
	if !s.engine.IsWindowOpen() {
		s.mu.Unlock()
		return errors.New("子窗口未打开；请先点「打开 1688」并搜到结果页")
	}
	s.running = true
	s.paused = false
	s.state = StateRunning
	s.results = nil
	s.ctx, s.cancel = context.WithCancel(context.Background())
	ctx := s.ctx
	s.mu.Unlock()

	pages := opts.Pages
	if pages <= 0 {
		pages = 3
	}
	if pages > 20 {
		pages = 20 // 上限，防误填
	}
	delay := time.Duration(opts.DelaySec) * time.Second
	if delay <= 0 {
		delay = 2 * time.Second
	}
	detailPerPage := opts.DetailPerPage
	if detailPerPage <= 0 {
		detailPerPage = 5
	}
	if detailPerPage > 10 {
		detailPerPage = 10 // 上限，防误填
	}
	doDetail := opts.DetailEnabled && s.detailEng != nil

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			s.running = false
			s.state = StateStopped
			s.mu.Unlock()
			s.emitState()
		}()

		s.logf(fmt.Sprintf("开始自动采集 %d 页（翻页间隔 %s，每页详情 %d 个 %s）",
			pages, delay, detailPerPage,
			func() string {
				if doDetail {
					return "（开详情窗补电话）"
				}
				return "（跳过详情窗）"
			}()))
		if doDetail && !s.detailEng.IsWindowOpen() {
			s.logf("⚠️ 详情窗未打开；将先自动开详情窗。首次使用请先在详情窗完成 1688 登录（滑块需手动过）。")
			_ = s.detailEng.OpenWindow(ctx, "")
			// 等详情窗加载完（bridge 注入循环会在 80ms 内重注）
			time.Sleep(2 * time.Second)
		}
		seenNames := map[string]bool{}
		var lastFingerprint string
		stuckCount := 0
		seenDetailURLs := map[string]bool{} // 跨页去重，同一条不重复开详情

		for p := 1; p <= pages; p++ {
			select {
			case <-ctx.Done():
				s.logf("已取消自动采集")
				return
			default:
			}

			s.logf(fmt.Sprintf("第 %d/%d 页：滚动加载…", p, pages))
			s.engine.ScrollToBottom(1200 * time.Millisecond)

			src := fmt.Sprintf("子窗口 第%d页", p)
			items := s.engine.CollectCurrentPage(ctx, src, func(m string) {
				s.logf(m)
			})

			// 累积去重（按名称+地址）
			added := 0
			s.mu.Lock()
			for _, it := range items {
				key := it.Name + "||" + it.Address
				if key == "||" || seenNames[key] {
					continue
				}
				seenNames[key] = true
				s.results = append(s.results, it)
				added++
			}
			all := make([]Supplier, len(s.results))
			copy(all, s.results)
			s.mu.Unlock()

			// ---- 双窗编排：补电话（可选）----
			// 对本页新加项里"有 URL 且未访问过"的，逐个 SetURL 到 detail 窗取电话。
			if doDetail && added > 0 {
				var toDetail []Supplier
				for _, it := range items {
					if it.URL == "" || seenDetailURLs[it.URL] {
						continue
					}
					seenDetailURLs[it.URL] = true
					toDetail = append(toDetail, it)
					if len(toDetail) >= detailPerPage {
						break
					}
				}
				if len(toDetail) > 0 {
					s.logf(fmt.Sprintf("第 %d/%d 页：补电话（详情窗访问 %d 条）", p, pages, len(toDetail)))
					for i, it := range toDetail {
						select {
						case <-ctx.Done():
							s.logf("已取消自动采集（详情循环中）")
							return
						default:
						}
						// 节奏：800ms + 0~400ms 抖动（模拟浏览）
						time.Sleep(800*time.Millisecond + time.Duration(rand.Intn(400))*time.Millisecond)
						if !s.detailEng.SetURL(ctx, it.URL) {
							s.logf("详情窗 SetURL 失败，跳过本条")
							continue
						}
						// 等导航稳定（bridge 80ms 重注 + 详情页加载）
						time.Sleep(2 * time.Second)
						detailItems := s.collectFromDetailWindow(ctx)
						phone := pickPhoneFromItems(detailItems, it)
						if phone != "" {
							s.mu.Lock()
							for k := range s.results {
								if s.results[k].URL == it.URL && s.results[k].Name == it.Name {
									s.results[k].Phone = phone
									break
								}
							}
							s.mu.Unlock()
							s.logf(fmt.Sprintf("详情窗 第%d/%d 条：补到电话 %s（%s）", i+1, len(toDetail), phone, it.Name))
						} else {
							// 被 baxia 拒 / 未取到 → 当轮详情停（避免反复撞风控）
							s.logf(fmt.Sprintf("详情窗 第%d/%d 条未取到电话（可能被 baxia 拦截）；当轮详情循环已停，列表采集继续", i+1, len(toDetail)))
							break
						}
						// 推送中间结果（补了电话的部分）
						s.mu.Lock()
						cur := make([]Supplier, len(s.results))
						copy(cur, s.results)
						s.mu.Unlock()
						if s.app != nil {
							_ = s.app.Event.Emit("c1688:result", cur)
						}
					}
				}
			}

			s.logf(fmt.Sprintf("第 %d/%d 页：新增 %d 条，累计 %d 条", p, pages, added, len(all)))
			if s.app != nil {
				_ = s.app.Event.Emit("c1688:result", all)
			}
			s.emitProgress()

			// MaxCount 已满 → 停
			if opts.MaxCount > 0 && len(all) >= opts.MaxCount {
				s.logf(fmt.Sprintf("已达 MaxCount=%d，提前结束", opts.MaxCount))
				s.store.Save(opts.Keyword, all)
				return
			}

			// 卡住检测：本页指纹（前 5 条名称拼接）与上页相同 → 翻页没成功 / 到底了
			fp := fingerprint(items)
			if p > 1 && fp == lastFingerprint {
				stuckCount++
				if stuckCount >= 1 {
					s.logf("连续页数据相同（翻页未成功或已到底），提前结束")
					break
				}
			} else {
				stuckCount = 0
			}
			lastFingerprint = fp

			// 最后一页不翻页
			if p == pages {
				break
			}

			s.logf("点「下一页」…")
			s.engine.ClickNextPage()
			// 等导航稳定 + 翻页间隔（防风控）
			time.Sleep(delay)
			// 再等 1s 让 bridge 重注 + 新文档加载
			time.Sleep(1 * time.Second)
		}

		s.mu.Lock()
		final := make([]Supplier, len(s.results))
		copy(final, s.results)
		s.mu.Unlock()
		s.store.Save(opts.Keyword, final)
		if s.app != nil {
			_ = s.app.Event.Emit("c1688:result", final)
		}
		s.logf(fmt.Sprintf("自动采集完成，共 %d 条", len(final)))
		s.emitProgress()
	}()
	s.emitState()
	return nil
}

// fingerprint 取一页数据前 5 条的名称做指纹（判断是否翻到相同页）。
func fingerprint(items []Supplier) string {
	var b strings.Builder
	for i := 0; i < len(items) && i < 5; i++ {
		b.WriteString(items[i].Name)
		b.WriteByte('|')
	}
	return b.String()
}

// OpenDetailWindow 打开（或聚焦）详情页子窗口。url 为空时到 1688 首页（供首登）。
// 用于"列表采集 + 详情补电话"双窗编排：用户先在本窗口登录 1688（登录态持久在 detail 引擎窗口），
// 之后 StartPages 会自动 SetURL 到各 item.Url 取电话。
func (s *Collect1688Service) OpenDetailWindow() error {
	if err := s.detailEng.OpenWindow(context.Background(), ""); err != nil {
		s.logf(err.Error())
		return err
	}
	s.logf("已打开详情页子窗口（1688 首页，供首登；登录态持久，下次免登）")
	return nil
}

// CloseDetailWindow 关闭详情页子窗口。
func (s *Collect1688Service) CloseDetailWindow() {
	s.detailEng.CloseWindow()
	s.logf("已关闭详情页子窗口")
}

// IsDetailWindowOpen 详情页子窗口是否还开着。
func (s *Collect1688Service) IsDetailWindowOpen() bool {
	return s.detailEng.IsWindowOpen()
}

// collectFromDetailWindow 把 detailEngine 当前页解析成供应商（用于从详情页补电话）。
// 返回 nil 表示取不到（窗口未开 / 回传超时 / 非详情页）。
func (s *Collect1688Service) collectFromDetailWindow(ctx context.Context) []Supplier {
	if !s.detailEng.IsWindowOpen() {
		return nil
	}
	return s.detailEng.CollectCurrentPage(ctx, "详情页", func(msg string) {
		s.logf(msg)
	})
}

// pickPhoneFromItems 从 detail 引擎采到的 items 里挑一个电话（供 StartPages 补电话用）。
// 优先 name 与 it.Name 匹配；否则取第一条非空 phone。
func pickPhoneFromItems(items []Supplier, it Supplier) string {
	for _, d := range items {
		if d.Phone != "" && it.Name != "" && d.Name == it.Name {
			return d.Phone
		}
	}
	for _, d := range items {
		if d.Phone != "" {
			return d.Phone
		}
	}
	return ""
}

// GetResults 取当前结果。
func (s *Collect1688Service) GetResults() []Supplier {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.results) == 0 {
		loaded := s.store.Load()
		if len(loaded) > 0 {
			s.results = loaded
			return s.results
		}
	}
	out := make([]Supplier, len(s.results))
	copy(out, s.results)
	return out
}

// GetLogs 取运行日志。
func (s *Collect1688Service) GetLogs() []LogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]LogEntry, len(s.logs))
	copy(out, s.logs)
	return out
}

// ExportCSV 导出当前结果为 CSV，返回文件绝对路径。
func (s *Collect1688Service) ExportCSV() (string, error) {
	items := s.GetResults()
	if len(items) == 0 {
		return "", errors.New("没有可导出的数据")
	}
	path, err := SaveCSV(items, s.store.ExportDir())
	if err != nil {
		return "", err
	}
	s.logf("已导出：" + path)
	return path, nil
}

// ListExports 列出已导出的 CSV 文件名。
func (s *Collect1688Service) ListExports() []string {
	return s.store.ListExports()
}

// ServiceShutdown 关闭后台 goroutine。
func (s *Collect1688Service) ServiceShutdown(app *application.App) error {
	s.cancel()
	s.wg.Wait()
	s.engine.CloseWindow()
	if s.detailEng != nil {
		s.detailEng.CloseWindow()
	}
	return nil
}
