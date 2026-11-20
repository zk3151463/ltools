package col1688

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

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
func NewCollect1688Service(app *application.App, dataDir string) *Collect1688Service {
	ctx, cancel := context.WithCancel(context.Background())
	svc := &Collect1688Service{
		app:     app,
		engine:  newWindowEngine(app, filepath.Join(dataDir, "diag")),
		store:   NewStore(dataDir),
		dataDir: dataDir,
		ctx:     ctx,
		cancel:  cancel,
		state:   StateIdle,
	}
	// 注入日志钩子，供 OnCreate 兜底钩子（关 WebView2 冒出的新窗）写日志
	svc.engine.SetLogf(svc.logf)
	// 注册子窗口回传 HTML 的事件监听 + 窗口创建兜底（必须在任何 OpenWindow 之前）
	svc.engine.registerEvents()
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
	return nil
}
