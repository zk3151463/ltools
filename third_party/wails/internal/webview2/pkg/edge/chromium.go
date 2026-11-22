//go:build windows
// +build windows

package edge

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/wailsapp/wails/v3/internal/webview2/internal/w32"
	"github.com/wailsapp/wails/v3/internal/webview2/webviewloader"
	"golang.org/x/sys/windows"
)

// ExtraInitScript 是应用层可设置的额外初始化脚本。
// 在 WebView2 初始化完成后，与 window.external.invoke 一起通过
// AddScriptToExecuteOnDocumentCreated 注入（跨导航，每个新文档都执行）。
// col1688 插件用它注入 bridgeJS + suppliersJS（不经过 ExecJS COM 调用，不碰 bad state）。
var ExtraInitScript string

type Rect = w32.Rect

func globalErrorHandler(err error) {
	if err == nil {
		return
	}

	log.Printf("[WebView2 Error] %v\n", err)

	stackBuf := make([]uintptr, 64)
	stackSize := runtime.Callers(2, stackBuf)
	frames := runtime.CallersFrames(stackBuf[:stackSize])

	log.Printf("\nStack trace:")
	stackIndex := 1
	for {
		frame, more := frames.Next()
		if !more {
			break
		}
		log.Printf("%d: %s\n\t%s:%d\n", stackIndex, frame.Function, frame.File, frame.Line)
		stackIndex++
	}
}

type Chromium struct {
	hwnd    uintptr
	padding struct {
		Left   int32
		Top    int32
		Right  int32
		Bottom int32
	}

	controller                       *ICoreWebView2Controller
	compositionController            *ICoreWebView2CompositionController
	compositionController4           *ICoreWebView2CompositionController4
	webview                          *ICoreWebView2
	inited                           uintptr
	initErr                          error
	envCompleted                     *iCoreWebView2CreateCoreWebView2EnvironmentCompletedHandler
	controllerCompleted              *iCoreWebView2CreateCoreWebView2ControllerCompletedHandler
	compositionControllerCompleted   *iCoreWebView2CreateCoreWebView2CompositionControllerCompletedHandler
	webMessageReceived               *iCoreWebView2WebMessageReceivedEventHandler
	containsFullScreenElementChanged *ICoreWebView2ContainsFullScreenElementChangedEventHandler
	permissionRequested              *iCoreWebView2PermissionRequestedEventHandler
	webResourceRequested             *iCoreWebView2WebResourceRequestedEventHandler
	acceleratorKeyPressed            *ICoreWebView2AcceleratorKeyPressedEventHandler
	moveFocusRequested               *ICoreWebView2MoveFocusRequestedEventHandler
	cursorChanged                    *iCoreWebView2CursorChangedEventHandler
	navigationStarting               *ICoreWebView2NavigationStartingEventHandler
	navigationCompleted              *ICoreWebView2NavigationCompletedEventHandler
	processFailed                    *ICoreWebView2ProcessFailedEventHandler
	newWindowRequested               *ICoreWebView2NewWindowRequestedEventHandler

	environment            *ICoreWebView2Environment
	webview2RuntimeVersion string
	compositionHost        *compositionHost

	// Settings
	Debug                         bool
	DataPath                      string
	BrowserPath                   string
	AdditionalBrowserArgs         []string
	NonClientRegionSupportEnabled bool
	CompositionControllerEnabled  bool

	// permissions
	permissions      map[CoreWebView2PermissionKind]CoreWebView2PermissionState
	globalPermission *CoreWebView2PermissionState

	// Callbacks
	MessageCallback                          func(message string, sender *ICoreWebView2, args *ICoreWebView2WebMessageReceivedEventArgs)
	MessageWithAdditionalObjectsCallback     func(message string, sender *ICoreWebView2, args *ICoreWebView2WebMessageReceivedEventArgs)
	WebResourceRequestedCallback             func(request *ICoreWebView2WebResourceRequest, args *ICoreWebView2WebResourceRequestedEventArgs)
	NavigationStartingCallback               func(sender *ICoreWebView2)
	NavigationCompletedCallback              func(sender *ICoreWebView2, args *ICoreWebView2NavigationCompletedEventArgs)
	ProcessFailedCallback                    func(sender *ICoreWebView2, args *ICoreWebView2ProcessFailedEventArgs)
	ContainsFullScreenElementChangedCallback func(sender *ICoreWebView2, args *ICoreWebView2ContainsFullScreenElementChangedEventArgs)
	AcceleratorKeyCallback                   func(uint) bool
	// MoveFocusRequestedCallback is called when the user tabs (or shift+tabs)
	// out of the WebView. Return true to take over focus handling (the
	// WebView will not wrap focus back to its own first/last element).
	MoveFocusRequestedCallback func(reason COREWEBVIEW2_MOVE_FOCUS_REASON) bool
	CursorChangedCallback      func(cursor HCURSOR, systemCursorID uint32)
	// NewWindowRequestedCallback is invoked when the page requests a new window
	// (target="_blank", window.open, <a target> on a popup). The host may
	// inspect args (GetUri, GetIsUserInitiated, GetWindowFeatures) and call
	// args.PutHandled(true) to suppress the default "open a new popup window".
	// Wired via AddNewWindowRequested in initializeController.
	NewWindowRequestedCallback func(sender *ICoreWebView2, args *ICoreWebView2NewWindowRequestedEventArgs)

	// Error handling
	globalErrorCallback func(error)

	shuttingDown bool

	// Resize debouncing
	lastBounds  *w32.Rect
	resizeTimer *time.Timer
}

func NewChromium() *Chromium {
	e := &Chromium{}
	/*
	 All these handlers are passed to native code through syscalls with 'uintptr(unsafe.Pointer(handler))' and we know
	 that a pointer to those will be kept in the native code. Furthermore these handlers als contain pointer to other Go
	 structs like the vtable.
	 This violates the unsafe.Pointer rule '(4) Conversion of a Pointer to a uintptr when calling syscall.Syscall.' because
	 theres no guarantee that Go doesn't move these objects.
	 AFAIK currently the Go runtime doesn't move HEAP objects, so we should be safe with these handlers. But they don't
	 guarantee it, because in the future Go might use a compacting GC.
	 There's a proposal to add a runtime.Pin function, to prevent moving pinned objects, which would allow to easily fix
	 this issue by just pinning the handlers. The https://go-review.googlesource.com/c/go/+/367296/ should land in Go 1.19.
	*/
	e.envCompleted = newICoreWebView2CreateCoreWebView2EnvironmentCompletedHandler(e)
	e.controllerCompleted = newICoreWebView2CreateCoreWebView2ControllerCompletedHandler(e)
	e.compositionControllerCompleted = newICoreWebView2CreateCoreWebView2CompositionControllerCompletedHandler(e)
	e.webMessageReceived = newICoreWebView2WebMessageReceivedEventHandler(e)
	e.permissionRequested = newICoreWebView2PermissionRequestedEventHandler(e)
	e.webResourceRequested = newICoreWebView2WebResourceRequestedEventHandler(e)
	e.acceleratorKeyPressed = newICoreWebView2AcceleratorKeyPressedEventHandler(e)
	e.moveFocusRequested = newICoreWebView2MoveFocusRequestedEventHandler(e)
	e.cursorChanged = newICoreWebView2CursorChangedEventHandler(e)
	e.navigationStarting = newICoreWebView2NavigationStartingEventHandler(e)
	e.navigationCompleted = newICoreWebView2NavigationCompletedEventHandler(e)
	e.processFailed = newICoreWebView2ProcessFailedEventHandler(e)
	e.newWindowRequested = NewICoreWebView2NewWindowRequestedEventHandler(e)
	e.containsFullScreenElementChanged = newICoreWebView2ContainsFullScreenElementChangedEventHandler(e)
	/*
		// Pinner seems to panic in some cases as reported on Discord, maybe during shutdown when GC detects pinned objects
		// to be released that have not been unpinned.
		// It would also be better to use our ComBridge for this event handlers implementation instead of pinning them.
		// So all COM Implementations on the go-side use the same code.
		var pinner runtime.Pinner
		pinner.Pin(e.envCompleted)
		pinner.Pin(e.controllerCompleted)
		pinner.Pin(e.webMessageReceived)
		pinner.Pin(e.permissionRequested)
		pinner.Pin(e.webResourceRequested)
		pinner.Pin(e.acceleratorKeyPressed)
		pinner.Pin(e.navigationCompleted)
		pinner.Pin(e.processFailed)
		pinner.Pin(e.containsFullScreenElementChanged)
	*/
	e.permissions = make(map[CoreWebView2PermissionKind]CoreWebView2PermissionState)
	e.globalErrorCallback = globalErrorHandler
	return e
}

func (e *Chromium) ShuttingDown() {
	e.shuttingDown = true
	atomic.StoreUintptr(&e.inited, 0)
	// WebView2 may keep this instance's handlers, and therefore the instance,
	// alive until it releases them. Drop the host callbacks so that retention
	// does not extend to the window that owned it.
	e.MessageCallback = nil
	e.MessageWithAdditionalObjectsCallback = nil
	e.WebResourceRequestedCallback = nil
	e.NavigationStartingCallback = nil
	e.NavigationCompletedCallback = nil
	e.ProcessFailedCallback = nil
	e.ContainsFullScreenElementChangedCallback = nil
	e.AcceleratorKeyCallback = nil
	e.MoveFocusRequestedCallback = nil
	e.CursorChangedCallback = nil
	e.NewWindowRequestedCallback = nil
	e.globalErrorCallback = globalErrorHandler
}

// Close abandons this instance and releases its controller and environment.
// It is safe after partial initialization and may be called more than once.
func (e *Chromium) Close() {
	e.ShuttingDown()
	e.ReleaseCompositionResources()
	if e.controller != nil {
		_ = e.controller.Close()
		e.controller.Release()
		e.controller = nil
	}
	if e.webview != nil {
		e.webview.Release()
		e.webview = nil
	}
	if e.environment != nil {
		e.environment.Release()
		e.environment = nil
	}
}

func (e *Chromium) errorCallback(err error) {
	e.globalErrorCallback(err)
	os.Exit(1)
}

func (e *Chromium) SetErrorCallback(callback func(error)) {
	if callback != nil {
		e.globalErrorCallback = callback
	}
}

func (e *Chromium) SetCursorChangedCallback(callback func(cursor HCURSOR, systemCursorID uint32)) {
	if callback != nil {
		e.CursorChangedCallback = callback
	}
}

// SetNewWindowRequestedCallback lets the host intercept "open a new window"
// requests (target="_blank", window.open, <a target>). The callback runs on
// the UI thread; call args.PutHandled(true) to suppress the default popup and
// optionally args.GetUri() to learn where the page wanted to go.
func (e *Chromium) SetNewWindowRequestedCallback(callback func(sender *ICoreWebView2, args *ICoreWebView2NewWindowRequestedEventArgs)) {
	if callback != nil {
		e.NewWindowRequestedCallback = callback
	}
}

// Embed retains the fatal startup behaviour for callers that require a webview.
func (e *Chromium) Embed(hwnd uintptr) bool {
	if err := e.EmbedWithError(hwnd); err != nil {
		e.errorCallback(err)
		return false
	}
	return true
}

// EmbedWithError creates a controller without terminating the process on failure.
// Use a fresh Chromium on the UI thread; a failed instance cannot be reused.
func (e *Chromium) EmbedWithError(hwnd uintptr) (err error) {
	defer func() {
		if err != nil {
			e.Close()
		}
	}()
	e.hwnd = hwnd

	dataPath := e.DataPath
	if dataPath == "" {
		currentExePath := make([]uint16, windows.MAX_PATH)
		_, err = windows.GetModuleFileName(windows.Handle(0), &currentExePath[0], windows.MAX_PATH)
		if err != nil {
			return err
		}
		currentExeName := filepath.Base(windows.UTF16ToString(currentExePath))
		dataPath = filepath.Join(os.Getenv("AppData"), currentExeName)
	}

	if e.BrowserPath != "" {
		if _, err = os.Stat(e.BrowserPath); err != nil {
			return fmt.Errorf("invalid browser path %q: %w", e.BrowserPath, err)
		}
	}

	// Check the runtime before starting asynchronous environment creation.
	e.webview2RuntimeVersion, err = webviewloader.GetAvailableCoreWebView2BrowserVersionString(e.BrowserPath)
	if err != nil {
		return fmt.Errorf("error getting Webview2 runtime version: %w", err)
	}
	browserArgs := strings.Join(e.AdditionalBrowserArgs, " ")
	if err := createCoreWebView2EnvironmentWithOptions(e.BrowserPath, dataPath, e.envCompleted, browserArgs); err != nil {
		return fmt.Errorf("error calling Webview2Loader: %w", err)
	}

	if !e.pumpUntilInited(embedTimeout) {
		if e.initErr != nil {
			return e.initErr
		}
		return fmt.Errorf("WebView2 controller did not complete initialisation within %s", embedTimeout)
	}
	// 注入 window.external.invoke（Wails 框架层通信）+ ExtraInitScript（应用层额外脚本）。
	// 关键：分多次 AddScriptToExecuteOnDocumentCreated 注册，互不影响。
	//   - 第 1 个脚本只定义 window.external（Wails 核心通信），绝不能让应用脚本破坏它。
	//   - 第 2 个脚本是 ExtraInitScript（col1688 的 bridgeJS + suppliersJS，约 15KB），包 try/catch，
	//     抛错不影响已注入的 window.external，且错误存到 window.__ltoolsInitErr 供诊断。
	//   - 每个脚本独立执行，避免"bridgeJS 语法错误 → 整个脚本（含 window.external）崩"的问题。
	e.Init("window.external={invoke:s=>window.chrome.webview.postMessage(s)}")
	if ExtraInitScript != "" {
		e.Init("window.__ltoolsInitErr='';try{" + ExtraInitScript + "}catch(e){window.__ltoolsInitErr=String(e)}")
	}
	return nil
}

// Initialization callbacks return to the message pump, which reports the first
// failure to EmbedWithError. They must stop before using incomplete COM objects.
func (e *Chromium) initializationFailed(err error) uintptr {
	if e.initErr == nil {
		e.initErr = err
	}
	return 0
}

// embedTimeout bounds how long Embed waits for CreateCoreWebView2Controller to
// complete. Controller creation is normally well under a second; this is a
// backstop for the case where the callback never arrives, not a budget.
const embedTimeout = 60 * time.Second

// pumpUntilInited runs a message loop until the controller is ready, the queue
// receives WM_QUIT, or timeout elapses. It reports whether the controller
// became ready.
func (e *Chromium) pumpUntilInited(timeout time.Duration) bool {
	var msg w32.Msg
	deadline := time.Now().Add(timeout)

	for {
		if e.initErr != nil || e.shuttingDown {
			return false
		}
		if atomic.LoadUintptr(&e.inited) != 0 {
			return true
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}

		// Wake on any queued message or when the deadline expires, whichever
		// comes first. Unlike GetMessageW this cannot park indefinitely.
		// MWMO_INPUTAVAILABLE is load-bearing: without it the wait ignores
		// messages already in the queue at entry, so a completion posted
		// between the drain below and this wait sleeps the full deadline.
		// (Note the Ex signature has no fWaitAll parameter.)
		r, _, _ := w32.User32MsgWaitForMultipleObjectsEx.Call(
			0, 0,
			uintptr(uint32(remaining.Milliseconds())),
			w32.QS_ALLINPUT,
			w32.MWMO_INPUTAVAILABLE,
		)
		if r == w32.WAIT_TIMEOUT {
			continue // re-check inited once more, then fail on the deadline
		}

		// MsgWaitForMultipleObjectsEx only signals that the queue is non-empty;
		// it does not remove anything. Drain what is there before waiting again,
		// or the next wait returns immediately on the same message.
		for {
			got, _, _ := w32.User32PeekMessageW.Call(
				uintptr(unsafe.Pointer(&msg)), 0, 0, 0, w32.PM_REMOVE,
			)
			if got == 0 {
				break
			}
			if msg.Message == w32.WM_QUIT {
				// Recovery may return to an outer application loop; preserve its quit.
				w32.User32PostQuitMessage.Call(msg.WParam)
				return false
			}
			w32.User32TranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
			w32.User32DispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
		}
	}
}

func (e *Chromium) SetPadding(padding Rect) {
	if e.padding.Left == padding.Left && e.padding.Top == padding.Top &&
		e.padding.Right == padding.Right && e.padding.Bottom == padding.Bottom {

		return
	}

	e.padding.Left = padding.Left
	e.padding.Top = padding.Top
	e.padding.Right = padding.Right
	e.padding.Bottom = padding.Bottom
	e.Resize()
}

func (e *Chromium) ResizeWithBounds(bounds *Rect) {
	if e.hwnd == 0 {
		return
	}

	bounds.Top += e.padding.Top
	bounds.Bottom -= e.padding.Bottom
	bounds.Left += e.padding.Left
	bounds.Right -= e.padding.Right

	e.SetSize(*bounds)
}

func (e *Chromium) Resize() {
	if e.hwnd == 0 {
		return
	}

	bounds, err := w32.GetClientRect(e.hwnd)
	if err != nil {
		// GetClientRect can fail transiently while the window is being torn
		// down or reconfigured during DPI churn. Skipping a resize frame is
		// recoverable; killing the process (errorCallback) is not.
		log.Printf("[WebView2] Resize failed to get client rect: %v", err)
		return
	}

	e.ResizeWithBounds(&bounds)
}

func (e *Chromium) Navigate(url string) {
	if e.webview == nil || e.shuttingDown {
		return
	}
	err := e.webview.Navigate(url)
	if err != nil {
		// A failed navigation is recoverable (the previous content stays
		// visible); killing the process is not.
		log.Printf("[WebView2] Navigate failed: %v", err)
	}
}

func (e *Chromium) NavigateToString(content string) {
	if e.webview == nil || e.shuttingDown {
		return
	}
	err := e.webview.NavigateToString(content)
	if err != nil {
		log.Printf("[WebView2] NavigateToString failed: %v", err)
	}
}

func (e *Chromium) Init(script string) {
	if e.webview == nil || e.shuttingDown {
		// 诊断：webview 为 nil 或正在关闭，Init 没执行（bridgeJS 不会注入）
		log.Printf("[WebView2] Init skipped（webviewNil=%v, shuttingDown=%v）", e.webview == nil, e.shuttingDown)
		return
	}
	// 诊断日志：确认 initScript 是否包含 ExtraInitScript（bridgeJS）
	// 搜 "ltoolsBridgeInstalled" 子串判断 bridgeJS 是否被拼进来
	hasBridge := strings.Contains(script, "ltoolsBridgeInstalled")
	log.Printf("[WebView2] Init script（len=%d, hasBridgeJS=%v, head=%s...）", len(script), hasBridge, func() string {
		if len(script) > 100 {
			return script[:100]
		}
		return script
	}())
	err := e.webview.AddScriptToExecuteOnDocumentCreated(script, nil)
	if err != nil {
		log.Printf("[WebView2] Init script registration failed: %v", err)
	} else {
		log.Printf("[WebView2] AddScriptToExecuteOnDocumentCreated OK（hasBridgeJS=%v）", hasBridge)
	}
}

func (e *Chromium) Eval(script string) {
	if e.webview == nil || e.shuttingDown {
		return
	}

	err := e.webview.ExecuteScript(script, nil)
	if err != nil && !errors.Is(err, windows.ERROR_IO_PENDING) {
		// ExecuteScript fails transiently while the browser process is busy
		// reconfiguring — e.g. RESOURCE_NOT_IN_CORRECT_STATE during a DPI
		// transition when the window is dragged between mixed-DPI monitors
		// (wailsapp/wails#5544). Script execution is fire-and-forget; a
		// dropped script during a transition is recoverable, killing the
		// process (errorCallback) is not.
		log.Printf("[WebView2] Eval failed: %v", err)
	}
}

// EvalWithResult 执行 JS 并**同步读回结果**（JSON 字符串）。
//
// 走 ICoreWebView2_21.ExecuteScriptWithResult（COM 回调），区别于 Eval（fire-and-forget，
// handler=nil 读不回结果）。JS 必须是"返回一个可 JSON 序列化值"的表达式（如
// JSON.stringify(...)），结果经 handler 回调里的 GetResultAsJson 读回。
//
// 线程模型（关键，根治死锁）：
//   - 调用方必须在**主线程**里调用本方法（execJSResult 经 InvokeSync 派发到主线程）。
//   - COM 完成回调经 windows.NewCallback（非 purego）注册，由 WebView2 在自己的 UI 线程
//     **同步**调用 Go 函数（不经 Wails 主线程消息泵）；回调里 close(e.evalDoneCh)。
//   - 若主线程在提交后阻塞等待（select / time.After / 10ms 轮询），主线程消息泵停转，
//     且回调虽能 close channel 但调用方已被阻塞 → 死锁/超时。
//   - 因此 EvalWithResult **不阻塞主线程**：提交 ExecuteScriptWithResult 后立即返回结果
//     channel；主线程调用方（execJSResult）把 channel 交回给**非主线程**的调用者
//     （public ExecJSResult）阻塞读取。COM 回调随时 close channel，非主线程读取即得结果。
//
// 返回的 channel 只发送一次（成功 / JS 失败 / 超时三选一），随后关闭。
//   - 成功：{res: JSON 字符串, err: nil}
//   - JS 抛异常 / Succeeded=false：{res: "", err: 非 nil}
//   - 超时：{res: "", err: ErrEvalTimeout}
//   - 提交失败（窗口未就绪 / ICoreWebView2_21 不支持）：{res: "", err: 非 nil}
var ErrEvalTimeout = errors.New("EvalWithResult: timeout")

// evalWithResultHandler 实现 iCoreWebView2ExecuteScriptWithResultCompletedHandlerImpl。
// COM 回调时：读 result.GetResultAsJson() → 存到 h.out → close(h.done)。
// 放堆上；生命周期由 handlerAddRef 记账（COM Release 前不被 GC，同 Wails 现有 CompletedHandler 模式，
// 见 com_refs.go）。
//
// done 是完成信号 channel（EvalWithResult 的局部 doneCh）：COM 回调（WebView2 UI 线程
// 同步调 Go）close 它；非主线程的等待 goroutine 收到即把结果送入 result channel。主线程不被阻塞。
type evalWithResultHandler struct {
	out  *EvalResult
	done chan struct{}
}

// EvalResult 承载一次 EvalWithResult 的结果，由 COM 回调写入、等待 goroutine 在 done 关闭后
// 送入结果 channel。导出供 pkg/application 的 execJSResult 读取。
type EvalResult struct {
	Res string
	Err error
}

func (h *evalWithResultHandler) QueryInterface(_ uintptr, object uintptr) uintptr {
	// WebView2 不会对 ExecuteScriptWithResult 的 handler 做 QueryInterface。
	// 返回 E_NOINTERFACE（0x80004002）。
	return 0x80004002
}

func (h *evalWithResultHandler) ExecuteScriptWithResultCompleted(errorCode uintptr, result *iCoreWebView2ExecuteScriptResult) uintptr {
	defer close(h.done)
	if int32(errorCode) < 0 || result == nil {
		h.out.Err = fmt.Errorf("ExecuteScriptWithResult completed with HRESULT 0x%08x", uint32(errorCode))
		return 0
	}
	succeeded, _ := result.GetSucceeded()
	if !succeeded {
		// 读异常信息（尽力而为，失败不影响主流程）
		msg := ""
		if ex, err := result.GetException(); err == nil && ex != nil {
			if line, lerr := ex.GetLineNumber(); lerr == nil && line > 0 {
				msg = fmt.Sprintf("（line %d）", line)
			}
		}
		h.out.Err = fmt.Errorf("JS 执行失败%s", msg)
		return 0
	}
	jsonStr, err := result.GetResultAsJson()
	if err != nil {
		// 退化：TryGetResultAsString（JS 结果是原始 string 时可直接读回）
		if s, ok, serr := result.TryGetResultAsString(); serr == nil && ok {
			h.out.Res = s
			return 0
		}
		h.out.Err = err
		return 0
	}
	h.out.Res = jsonStr
	return 0
}

// EvalWithResult 执行 JS 并提交**异步读回**（返回结果 channel，非阻塞）。
//
// 调用方必须在 **Wails 主线程** 里调用本方法（execJSResult 经 InvokeSync 派发到主线程）。
// 本方法只提交 COM 调用 + 启动等待 goroutine，**立即返回结果 channel**，绝不阻塞主线程。
// 主线程调用方（execJSResult）拿到 channel 后交给**非主线程**的 public ExecJSResult 阻塞
// 读取——此时主线程消息泵继续转，COM 完成回调随时 close evalDoneCh，等待 goroutine 把结果
// 送入 channel。
//
// 调用方必须保证：同窗口同一时刻只有一个 EvalWithResult 在飞（col1688 的单窗口串行调用满足）。
// 返回的 channel 只发送一次（成功 / JS 失败 / 超时三选一），随后关闭。
func (e *Chromium) EvalWithResult(script string, timeout time.Duration) chan EvalResult {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	// 结果 channel（容量 1，保证等待 goroutine 能发出不阻塞）。
	result := make(chan EvalResult, 1)

	if e.webview == nil || e.shuttingDown {
		// 窗口未就绪：直接返回错误结果，不等超时。
		result <- EvalResult{Err: errors.New("webview not initialized or shutting down")}
		close(result)
		return result
	}
	// ICoreWebView2_21 接口（ExecuteScriptWithResult 在它上面）。
	// 老版 WebView2 运行时 QueryInterface 失败 → 回退 Eval（fire-and-forget，读不回结果）。
	wv21 := e.webview.GetICoreWebView2_21()
	if wv21 == nil {
		e.Eval(script)
		result <- EvalResult{Err: errors.New("WebView2 ICoreWebView2_21 not supported (old runtime); fell back to Eval")}
		close(result)
		return result
	}

	// 重建完成信号 channel（每个调用独立；COM 回调 close 它）。
	doneCh := make(chan struct{})

	// handler 放堆上（vtbl 指向包级静态 vtbl，impl 是 &handler 自身）；
	// handlerAddRef 记账保证 COM Release 前不被 GC（同 Wails 现有 handler 模式）。
	out := &EvalResult{}
	impl := &evalWithResultHandler{out: out, done: doneCh}
	handler := newICoreWebView2ExecuteScriptWithResultCompletedHandler(impl)
	handlerAddRef(unsafe.Pointer(handler))

	// Transient COM errors (e.g. RESOURCE_NOT_IN_CORRECT_STATE during
	// dual-window concurrent activity, DPI transition, or page navigation)
	// are retried. Wails' own transient-error philosophy (see Eval / SetSize /
	// PutBounds): "dropped/failed call during reconfiguration is recoverable".
	// For synchronous read-back we retry up to 2 times (3 total attempts).
	//
	// Retry is safe: if the error is persistent (UTF16 encode failure, etc.),
	// the retry will fail immediately on the same condition and we exit after
	// 3 total attempts. If transient, the backoff lets WebView2 settle.
	const maxAttempts = 2
	var submitErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// Backoff: let WebView2 recover from RESOURCE_NOT_IN_CORRECT_STATE.
			// 1s is enough for navigation/DPI to settle; 200ms was too short
			// (WebView2 still in bad state → retry also fails).
			time.Sleep(1 * time.Second)
			wv21.Release()
			wv21 = e.webview.GetICoreWebView2_21()
			if wv21 == nil {
				// Old runtime: fall back to Eval (fire-and-forget, no result).
				e.Eval(script)
				handlerRelease(unsafe.Pointer(handler))
				result <- EvalResult{Err: errors.New("WebView2 ICoreWebView2_21 not supported (old runtime); fell back to Eval")}
				close(result)
				return result
			}
		}
		submitErr = wv21.ExecuteScriptWithResult(script, handler)
		if submitErr == nil {
			break
		}
		// Non-Win32 error (Go-level, e.g. UTF16PtrFromString) → no point retrying.
		if _, isWinErrno := submitErr.(windows.Errno); !isWinErrno {
			break
		}
	}
	if submitErr != nil {
		handlerRelease(unsafe.Pointer(handler))
		wv21.Release()
		result <- EvalResult{Err: fmt.Errorf("ExecuteScriptWithResult: %w (retried %d times)", submitErr, maxAttempts-1)}
		close(result)
		return result
	}
	wv21.Release()

	// 非主线程等待：COM 回调 close(doneCh) 或超时，二者先到者发送结果。
	// handler 的生命周期：等待 goroutine 完成（收到结果或超时）后才 handlerRelease，
	// 避免迟到的 COM 回调访问已回收的 handler。
	go func() {
		defer handlerRelease(unsafe.Pointer(handler))
		select {
		case <-doneCh:
			// COM 回调已把结果写入 out。
			result <- *out
		case <-time.After(timeout):
			result <- EvalResult{Err: ErrEvalTimeout}
			// 超时后 COM 回调可能稍后才 close(doneCh)；doneCh 是独立局部变量，
			// 无数据竞争；迟到的回调会尝试写入 out（已无人读，安全 no-op）。
		}
		close(result)
	}()
	return result
}

func (e *Chromium) Show() error {
	if e.controller == nil || e.shuttingDown {
		return nil
	}
	return e.controller.PutIsVisible(true)
}

func (e *Chromium) Hide() error {
	if e.controller == nil || e.shuttingDown {
		return nil
	}
	return e.controller.PutIsVisible(false)
}

// Handler queries are not expected; count them so tests notice a native
// reference that bypasses AddRef.
var handlerQueryInterfaces atomic.Uint64

func (e *Chromium) QueryInterface(_, _ uintptr) uintptr {
	handlerQueryInterfaces.Add(1)
	return 0
}

func (e *Chromium) EnvironmentCompleted(res uintptr, env *ICoreWebView2Environment) uintptr {
	if e.shuttingDown || e.initErr != nil {
		return 0
	}
	if int32(res) < 0 || env == nil {
		return e.initializationFailed(fmt.Errorf("failed to create WebView2 environment: HRESULT 0x%08x", res))
	}

	log.Printf("[WebView2] Environment created successfully\n")

	env.AddRef()
	e.environment = env

	var err error
	if !e.CompositionControllerEnabled {
		err = env.CreateCoreWebView2Controller(e.hwnd, e.controllerCompleted)
	} else {
		err = e.createCoreWebView2CompositionController(env)
		if err != nil {
			err = e.fallbackToCoreWebView2Controller(fmt.Errorf("composition controller setup failed: %w", err))
		}
	}
	if err != nil {
		return e.initializationFailed(err)
	}
	return 0
}

func (e *Chromium) CreateCoreWebView2ControllerCompleted(res uintptr, controller *ICoreWebView2Controller) uintptr {
	if e.shuttingDown || e.initErr != nil {
		if controller != nil {
			_ = controller.Close()
		}
		return 0
	}
	if int32(res) < 0 || controller == nil {
		return e.initializationFailed(fmt.Errorf("error creating controller with %08x: %s", res, syscall.Errno(res)))
	}

	return e.initializeController(controller)
}

func (e *Chromium) createCoreWebView2CompositionController(env *ICoreWebView2Environment) error {
	env3 := env.GetICoreWebView2Environment3()
	if env3 == nil {
		return UnsupportedCapabilityError
	}
	defer env3.Release()

	host, err := newCompositionHost(e.hwnd)
	if err != nil {
		return err
	}
	e.compositionHost = host

	return env3.CreateCoreWebView2CompositionController(e.hwnd, e.compositionControllerCompleted)
}

func (e *Chromium) CreateCoreWebView2CompositionControllerCompleted(res uintptr, compositionController *ICoreWebView2CompositionController) uintptr {
	if e.shuttingDown || e.initErr != nil {
		if compositionController != nil {
			if controller := compositionController.GetICoreWebView2Controller(); controller != nil {
				_ = controller.Close()
				controller.Release()
			}
		}
		return 0
	}
	if int32(res) < 0 {
		if err := e.fallbackToCoreWebView2Controller(fmt.Errorf("error creating composition controller with %08x: %s", res, syscall.Errno(res))); err != nil {
			return e.initializationFailed(err)
		}
		return 0
	}

	if compositionController == nil {
		if err := e.fallbackToCoreWebView2Controller(fmt.Errorf("composition controller completed without a controller")); err != nil {
			return e.initializationFailed(err)
		}
		return 0
	}

	compositionController.AddRef()
	e.compositionController = compositionController
	e.compositionController4 = compositionController.GetICoreWebView2CompositionController4()

	if err := e.compositionHost.attachController(e.compositionController); err != nil {
		if fallbackErr := e.fallbackToCoreWebView2Controller(fmt.Errorf("attaching composition controller failed: %w", err)); fallbackErr != nil {
			return e.initializationFailed(fallbackErr)
		}
		return 0
	}

	controller := compositionController.GetICoreWebView2Controller()
	if controller == nil {
		if err := e.fallbackToCoreWebView2Controller(fmt.Errorf("error getting controller from composition controller")); err != nil {
			return e.initializationFailed(err)
		}
		return 0
	}

	defer controller.Release()
	return e.initializeController(controller)
}

func (e *Chromium) fallbackToCoreWebView2Controller(reason error) error {
	log.Printf("[WebView2] Composition hosting unavailable, falling back to HWND controller: %v\n", reason)

	e.releaseCompositionController()
	e.releaseCompositionHost()
	e.CompositionControllerEnabled = false

	if e.environment == nil {
		return fmt.Errorf("cannot fall back to WebView2 HWND controller without an environment: %w", reason)
	}
	if err := e.environment.CreateCoreWebView2Controller(e.hwnd, e.controllerCompleted); err != nil {
		return fmt.Errorf("composition hosting failed (%v), and HWND controller fallback failed: %w", reason, err)
	}
	return nil
}

// ReleaseCompositionResources drops the composition controller and the DComp
// host targeting this Chromium's HWND. An HWND can carry only one DComp
// target: while the old one exists, a replacement Chromium's composition
// setup fails with DCOMPOSITION_ERROR_WINDOW_ALREADY_COMPOSED and silently
// falls back to windowed hosting. A controller rebuild must call this on the
// abandoned instance before creating its replacement.
func (e *Chromium) ReleaseCompositionResources() {
	e.releaseCompositionController()
	e.releaseCompositionHost()
}

func (e *Chromium) releaseCompositionController() {
	if e.compositionController4 != nil {
		e.compositionController4.Release()
		e.compositionController4 = nil
	}
	if e.compositionController != nil {
		if controller := e.compositionController.GetICoreWebView2Controller(); controller != nil {
			_ = controller.Close()
			controller.Release()
		}
		e.compositionController.Release()
		e.compositionController = nil
	}
}

func (e *Chromium) releaseCompositionHost() {
	if e.compositionHost != nil {
		e.compositionHost.release()
		e.compositionHost = nil
	}
}

func (e *Chromium) initializeController(controller *ICoreWebView2Controller) uintptr {
	var err error

	controller.AddRef()
	e.controller = controller

	// Try to get ICoreWebView2Controller3 interface for better performance.
	// Keep composition-hosted WebViews on WebView2-managed scale detection:
	// the DirectComposition surface is owned by WebView2, and manually
	// reasserting RasterizationScale during monitor transitions can leave that
	// surface black after DPI increases.
	if controller3 := e.controller.GetICoreWebView2Controller3(); controller3 != nil {
		defer func() { controller3.Vtbl.Release.Call(uintptr(unsafe.Pointer(controller3))) }()
		if !e.CompositionControllerEnabled {
			// Use raw pixels mode for better performance during resize.
			if err := controller3.PutBoundsMode(COREWEBVIEW2_BOUNDS_MODE_USE_RAW_PIXELS); err != nil {
				return e.initializationFailed(err)
			}

			// ShouldDetectMonitorScaleChanges is deliberately left at its default
			// (enabled): WebView2 tracks monitor DPI changes and updates its own
			// rasterization scale. This module previously disabled it and left the
			// scale to the host's WM_DPICHANGED handling, but an externally
			// written scale races the browser process's internal display
			// bookkeeping during a mixed-DPI monitor cross — the browser can land
			// on a degenerate scale(0,0) compositor transform, which kills the
			// GPU process on every frame until the browser process itself exits
			// (wailsapp/wails#5732). Detection stays on so the scale has exactly
			// one writer, on the code path every mainstream embedder exercises;
			// bounds remain raw pixels, which is orthogonal.
		}
	}
	var token _EventRegistrationToken
	e.webview, err = e.controller.GetCoreWebView2()
	if err != nil {
		return e.initializationFailed(err)
	}

	if e.NonClientRegionSupportEnabled {
		if err := e.PutIsNonClientRegionSupportEnabled(true); err != nil {
			if !errors.Is(err, UnsupportedCapabilityError) {
				return e.initializationFailed(err)
			}
		}
	}
	err = e.webview.AddWebMessageReceived(e.webMessageReceived, &token)
	if err != nil {
		return e.initializationFailed(err)
	}
	err = e.webview.AddPermissionRequested(e.permissionRequested, &token)
	if err != nil {
		return e.initializationFailed(err)
	}
	err = e.webview.AddWebResourceRequested(e.webResourceRequested, &token)
	if err != nil {
		return e.initializationFailed(err)
	}
	err = e.webview.AddNavigationStarting(e.navigationStarting, &token)
	if err != nil {
		return e.initializationFailed(err)
	}
	err = e.webview.AddNewWindowRequested(e.newWindowRequested, &token)
	if err != nil {
		return e.initializationFailed(err)
	}
	err = e.webview.AddNavigationCompleted(e.navigationCompleted, &token)
	if err != nil {
		return e.initializationFailed(err)
	}
	err = e.webview.AddProcessFailed(e.processFailed, &token)
	if err != nil {
		return e.initializationFailed(err)
	}
	err = e.webview.AddContainsFullScreenElementChanged(e.containsFullScreenElementChanged, &token)
	if err != nil {
		return e.initializationFailed(err)
	}
	err = e.controller.AddAcceleratorKeyPressed(e.acceleratorKeyPressed, &token)
	if err != nil {
		return e.initializationFailed(err)
	}
	// Without this handler WebView2 wraps Tab focus around inside the page,
	// so keyboard focus can never leave the WebView (wailsapp/wails#3783).
	err = e.controller.AddMoveFocusRequested(e.moveFocusRequested, &token)
	if err != nil {
		return e.initializationFailed(err)
	}
	if e.compositionController != nil {
		err = e.compositionController.AddCursorChanged(e.cursorChanged, &token)
		if err != nil {
			return e.initializationFailed(err)
		}
	}

	atomic.StoreUintptr(&e.inited, 1)

	return 0
}

func (e *Chromium) ContainsFullScreenElementChanged(sender *ICoreWebView2, args *ICoreWebView2ContainsFullScreenElementChangedEventArgs) uintptr {
	if e.ContainsFullScreenElementChangedCallback != nil {
		e.ContainsFullScreenElementChangedCallback(sender, args)
	}
	return 0
}

func (e *Chromium) CursorChanged(sender *ICoreWebView2CompositionController, _ *IUnknown) uintptr {
	if e.CursorChangedCallback == nil {
		return 0
	}

	// A cursor that cannot be read leaves the previous cursor showing until
	// the next change; that is recoverable, killing the process
	// (errorCallback) is not (wailsapp/wails#5580).
	cursor, err := sender.GetCursor()
	if err != nil {
		log.Printf("[WebView2] CursorChanged failed to get cursor: %v", err)
		return 0
	}

	systemCursorID, err := sender.GetSystemCursorId()
	if err != nil {
		log.Printf("[WebView2] CursorChanged failed to get system cursor id: %v", err)
		return 0
	}

	e.CursorChangedCallback(cursor, systemCursorID)
	return 0
}

func (e *Chromium) MessageReceived(sender *ICoreWebView2, args *ICoreWebView2WebMessageReceivedEventArgs) uintptr {
	message, err := args.TryGetWebMessageAsString()
	if err != nil {
		// The web message originates from (potentially untrusted) page
		// content. A malformed message must never be able to take the whole
		// process down — drop it and keep running.
		log.Printf("[WebView2] dropping malformed web message: %v", err)
		return 0
	}

	if HasCapability(e.webview2RuntimeVersion, GetAdditionalObjects) {
		obj, err := args.GetAdditionalObjects()
		if err != nil {
			// Fall back to delivering the plain string message below rather
			// than killing the process.
			log.Printf("[WebView2] failed to read additional objects, delivering message without them: %v", err)
			obj = nil
		}

		if obj != nil && e.MessageWithAdditionalObjectsCallback != nil {
			defer obj.Release()
			e.MessageWithAdditionalObjectsCallback(message, sender, args)
		} else if e.MessageCallback != nil {
			e.MessageCallback(message, sender, args)
		}
	} else if e.MessageCallback != nil {
		e.MessageCallback(message, sender, args)
	}

	err = sender.PostWebMessageAsString(message)
	if err != nil && !errors.Is(err, windows.ERROR_IO_PENDING) {
		log.Printf("[WebView2] PostWebMessageAsString failed: %v", err)
	}
	return 0
}

func (e *Chromium) SetPermission(kind CoreWebView2PermissionKind, state CoreWebView2PermissionState) {
	e.permissions[kind] = state
}

func (e *Chromium) SetBackgroundColour(R, G, B, A uint8) {
	if e.shuttingDown {
		return
	}
	if err := e.SetBackgroundColourWithError(R, G, B, A); err != nil {
		// The window keeps its previous background colour; that is
		// recoverable, killing the process is not (wailsapp/wails#5580).
		// Setup uses SetBackgroundColourWithError to see the error.
		log.Printf("[WebView2] SetBackgroundColour failed: %v", err)
	}
}

// SetBackgroundColourWithError applies the colour without terminating the app.
func (e *Chromium) SetBackgroundColourWithError(R, G, B, A uint8) error {
	controller := e.GetController()
	if controller == nil {
		return errors.New("webview2 controller is not initialized")
	}
	controller2 := controller.GetICoreWebView2Controller2()
	if controller2 == nil {
		return UnsupportedCapabilityError
	}
	defer controller2.vtbl.Release.Call(uintptr(unsafe.Pointer(controller2)))

	backgroundCol := COREWEBVIEW2_COLOR{
		A: A,
		R: R,
		G: G,
		B: B,
	}

	// WebView2 only has 0 and 255 as valid values.
	if backgroundCol.A > 0 && backgroundCol.A < 255 {
		backgroundCol.A = 255
	}

	return controller2.PutDefaultBackgroundColor(backgroundCol)
}

func (e *Chromium) SetGlobalPermission(state CoreWebView2PermissionState) {
	e.globalPermission = &state
}

func (e *Chromium) PermissionRequested(_ *ICoreWebView2, args *iCoreWebView2PermissionRequestedEventArgs) uintptr {
	var result CoreWebView2PermissionState
	kind, err := args.GetPermissionKind()
	if err != nil {
		// A request whose kind cannot be read cannot match a per-kind rule,
		// so it is denied unless a global permission applies; denying one
		// request is recoverable, killing the process is not
		// (wailsapp/wails#5580).
		log.Printf("[WebView2] PermissionRequested failed to get permission kind: %v", err)
		result = CoreWebView2PermissionStateDeny
		if e.globalPermission != nil {
			result = *e.globalPermission
		}
	} else if e.globalPermission != nil {
		result = *e.globalPermission
	} else {
		var ok bool
		result, ok = e.permissions[kind]
		if !ok {
			result = CoreWebView2PermissionStateDefault
		}
	}
	err = args.PutState(result)
	if err != nil {
		// WebView2 then applies its default handling to this request.
		log.Printf("[WebView2] PermissionRequested failed to set state: %v", err)
	}
	return 0
}

func (e *Chromium) WebResourceRequested(sender *ICoreWebView2, args *ICoreWebView2WebResourceRequestedEventArgs) uintptr {
	req, err := args.GetRequest()
	if err != nil || req == nil {
		// COM can fail here under load without setting the out pointer
		// (#1103). Dropping one request is recoverable (the WebView falls
		// back to default handling for it); killing the process is not.
		if err == nil {
			err = errors.New("no error, but the request out pointer was not set (#1103)")
		}
		log.Printf("[WebView2] WebResourceRequested failed to get request: %v", err)
		return 0
	}
	defer req.Release()

	if e.WebResourceRequestedCallback != nil {
		e.WebResourceRequestedCallback(req, args)
	}
	return 0
}

func (e *Chromium) AddWebResourceRequestedFilter(filter string, ctx COREWEBVIEW2_WEB_RESOURCE_CONTEXT) {
	if err := e.AddWebResourceRequestedFilterWithError(filter, ctx); err != nil {
		// Requests matching the filter fall back to WebView2's default
		// handling; that is recoverable, killing the process is not
		// (wailsapp/wails#5580). Setup uses
		// AddWebResourceRequestedFilterWithError to see the error.
		log.Printf("[WebView2] AddWebResourceRequestedFilter failed: %v", err)
	}
}

// AddWebResourceRequestedFilterWithError reports setup errors to the caller.
func (e *Chromium) AddWebResourceRequestedFilterWithError(filter string, ctx COREWEBVIEW2_WEB_RESOURCE_CONTEXT) error {
	if e.webview == nil {
		return errors.New("webview2 is not initialized")
	}
	return e.webview.AddWebResourceRequestedFilter(filter, ctx)
}

func (e *Chromium) Environment() *ICoreWebView2Environment {
	return e.environment
}

// AcceleratorKeyPressed is called when an accelerator key is pressed.
// If the AcceleratorKeyCallback method has been set, it will defer handling of the keypress
// to the callback. That callback returns a bool indicating if the event was handled.
func (e *Chromium) AcceleratorKeyPressed(sender *ICoreWebView2Controller, args *ICoreWebView2AcceleratorKeyPressedEventArgs) uintptr {
	if e.AcceleratorKeyCallback == nil {
		return 0
	}
	eventKind, _ := args.GetKeyEventKind()
	if eventKind == COREWEBVIEW2_KEY_EVENT_KIND_KEY_DOWN ||
		eventKind == COREWEBVIEW2_KEY_EVENT_KIND_SYSTEM_KEY_DOWN {
		virtualKey, _ := args.GetVirtualKey()
		status, _ := args.GetPhysicalKeyStatus()
		if !status.WasKeyDown {
			err := args.PutHandled(e.AcceleratorKeyCallback(virtualKey))
			if err != nil {
				// The key then takes WebView2's default path; that is
				// recoverable, killing the process on a keypress is not
				// (wailsapp/wails#5580).
				log.Printf("[WebView2] AcceleratorKeyPressed failed to set handled: %v", err)
			}
		} else {
			return 0
		}
	}
	err := args.PutHandled(false)
	if err != nil {
		log.Printf("[WebView2] AcceleratorKeyPressed failed to set handled: %v", err)
	}
	return 0
}

// MoveFocusRequested is called when the user tabs out of the WebView. If
// MoveFocusRequestedCallback reports the request as handled, WebView2's default
// behaviour (wrapping focus inside the page) is suppressed.
func (e *Chromium) MoveFocusRequested(sender *ICoreWebView2Controller, args *ICoreWebView2MoveFocusRequestedEventArgs) uintptr {
	handled := false
	if !e.shuttingDown && e.MoveFocusRequestedCallback != nil {
		if reason, err := args.GetReason(); err == nil {
			handled = e.MoveFocusRequestedCallback(reason)
		}
	}
	if err := args.PutHandled(handled); err != nil {
		log.Printf("[WebView2] MoveFocusRequested PutHandled failed: %v", err)
	}
	return 0
}

// MoveFocus moves focus into the WebView for the given reason. Use
// COREWEBVIEW2_MOVE_FOCUS_REASON_NEXT / _PREVIOUS to enter at the first / last
// focusable element when tabbing in from the host window.
func (e *Chromium) MoveFocus(reason COREWEBVIEW2_MOVE_FOCUS_REASON) {
	if !e.IsReady() {
		return
	}
	if err := e.controller.MoveFocus(reason); err != nil {
		log.Printf("[WebView2] MoveFocus failed: %v", err)
	}
}

func (e *Chromium) GetSettings() (*ICoreWebViewSettings, error) {
	if e.webview == nil {
		return nil, errors.New("webview2 is not initialized")
	}
	return e.webview.GetSettings()
}

func (e *Chromium) GetController() *ICoreWebView2Controller {
	return e.controller
}

// IsReady reports whether the WebView2 controller has been fully initialised.
// e.controller is assigned partway through CreateCoreWebView2ControllerCompleted,
// before the controller's COM setup has finished, so a non-nil controller is
// not sufficient to safely call into it: COM calls made in that window fail
// with E_INVALIDARG ("The parameter is incorrect"). The inited flag is set
// only after setup completes. Safe to call from any goroutine.
func (e *Chromium) IsReady() bool {
	return atomic.LoadUintptr(&e.inited) != 0
}

func boolToInt(input bool) int {
	if input {
		return 1
	}
	return 0
}

func (e *Chromium) NavigationStarting(sender *ICoreWebView2, _ *IUnknown) uintptr {
	if !e.shuttingDown && e.NavigationStartingCallback != nil {
		e.NavigationStartingCallback(sender)
	}
	return 0
}

// NewWindowRequested is the COM callback for ICoreWebView2NewWindowRequestedEventHandler.
// It runs on the UI thread when the page requests a new window (target="_blank",
// window.open, <a target>). The host callback may inspect args and call
// args.PutHandled(true) to suppress the default popup.
func (e *Chromium) NewWindowRequested(sender *ICoreWebView2, args *ICoreWebView2NewWindowRequestedEventArgs) uintptr {
	if !e.shuttingDown && e.NewWindowRequestedCallback != nil {
		e.NewWindowRequestedCallback(sender, args)
	}
	return 0
}

func (e *Chromium) NavigationCompleted(sender *ICoreWebView2, args *ICoreWebView2NavigationCompletedEventArgs) uintptr {
	if !e.shuttingDown && e.NavigationCompletedCallback != nil {
		e.NavigationCompletedCallback(sender, args)
	}
	return 0
}

func (e *Chromium) ProcessFailed(sender *ICoreWebView2, args *ICoreWebView2ProcessFailedEventArgs) uintptr {
	if !e.shuttingDown && e.ProcessFailedCallback != nil {
		e.ProcessFailedCallback(sender, args)
	}
	return 0
}

func (e *Chromium) Bounds() *Rect {
	if e == nil || e.controller == nil {
		return nil
	}

	rect, err := e.controller.GetBounds()
	if err != nil {
		// nil already means "bounds unavailable" (see above); a failed read
		// is recoverable, killing the process is not (wailsapp/wails#5580).
		log.Printf("[WebView2] Bounds failed: %v", err)
		return nil
	}
	return rect
}

func (e *Chromium) NotifyParentWindowPositionChanged() error {
	//It looks like the wndproc function is called before the controller initialization is complete.
	//Because of this the controller is nil
	if e.controller == nil {
		return nil
	}
	return e.controller.NotifyParentWindowPositionChanged()
}

func (e *Chromium) Focus() {
	// The WndProc can dispatch WM_SETFOCUS re-entrantly while the controller
	// is still being configured in CreateCoreWebView2ControllerCompleted
	// (issue #5446). Callers' GetController() != nil checks cannot exclude
	// that window, so guard here: dropping a focus request during startup is
	// harmless, calling MoveFocus on a partially-initialised controller is
	// fatal (errorCallback exits the process).
	if !e.IsReady() {
		return
	}
	err := e.controller.MoveFocus(COREWEBVIEW2_MOVE_FOCUS_REASON_PROGRAMMATIC)
	if err != nil {
		// MoveFocus can legitimately fail after initialisation too — e.g.
		// E_INVALIDARG when the window is hidden or minimised to the tray
		// (wailsapp/wails#4158 reproduces this on tray-click restore). A
		// failed focus request is never worth killing the process, which is
		// what errorCallback does; log it instead.
		log.Printf("[WebView2] Focus failed: %v", err)
	}
}

func (e *Chromium) PutZoomFactor(zoomFactor float64) {
	if e.controller == nil || e.shuttingDown {
		return
	}
	err := e.controller.PutZoomFactor(zoomFactor)
	if err != nil {
		log.Printf("[WebView2] PutZoomFactor failed: %v", err)
	}
}

func (e *Chromium) OpenDevToolsWindow() {
	if e.webview == nil || e.shuttingDown {
		return
	}
	err := e.webview.OpenDevToolsWindow()
	if err != nil {
		log.Printf("[WebView2] OpenDevToolsWindow failed: %v", err)
	}
}

func (e *Chromium) HasCapability(c Capability) bool {
	return HasCapability(e.webview2RuntimeVersion, c)
}

func (e *Chromium) CompositionControllerReady() bool {
	return e != nil && e.compositionController != nil
}

func (e *Chromium) NonClientRegionHitTestReady() bool {
	return e != nil &&
		e.compositionController4 != nil &&
		HasCapability(e.webview2RuntimeVersion, NonClientRegion)
}

func (e *Chromium) GetIsSwipeNavigationEnabled() (bool, error) {
	if !HasCapability(e.webview2RuntimeVersion, SwipeNavigation) {
		return false, UnsupportedCapabilityError
	}
	webview2Settings, err := e.webview.GetSettings()
	if err != nil {
		return false, err
	}
	webview2Settings6 := webview2Settings.GetICoreWebView2Settings6()
	var result bool
	result, err = webview2Settings6.GetIsSwipeNavigationEnabled()
	if err != nil {
		return false, err
	}
	return result, nil
}

// PutIsGeneralAutofillEnabled controls whether autofill for information
// like names, street and email addresses, phone numbers, and arbitrary input
// is enabled. This excludes password and credit card information. When
// IsGeneralAutofillEnabled is false, no suggestions appear, and no new information
// is saved. When IsGeneralAutofillEnabled is true, information is saved, suggestions
// appear and clicking on one will populate the form fields.
// It will take effect immediately after setting.
// The default value is `FALSE`.
func (e *Chromium) PutIsGeneralAutofillEnabled(value bool) error {
	if !HasCapability(e.webview2RuntimeVersion, GeneralAutofillEnabled) {
		return UnsupportedCapabilityError
	}
	webview2Settings, err := e.webview.GetSettings()
	if err != nil {
		return err
	}
	webview2Settings4 := webview2Settings.GetICoreWebView2Settings4()
	return webview2Settings4.PutIsGeneralAutofillEnabled(value)
}

// PutIsPasswordAutosaveEnabled sets whether the browser should offer to save passwords and other
// identifying information entered into forms automatically.
// The default value is `FALSE`.
func (e *Chromium) PutIsPasswordAutosaveEnabled(value bool) error {
	if !HasCapability(e.webview2RuntimeVersion, PasswordAutosaveEnabled) {
		return UnsupportedCapabilityError
	}
	webview2Settings, err := e.webview.GetSettings()
	if err != nil {
		return err
	}
	webview2Settings4 := webview2Settings.GetICoreWebView2Settings4()
	return webview2Settings4.PutIsPasswordAutosaveEnabled(value)
}

func (e *Chromium) PutIsSwipeNavigationEnabled(enabled bool) error {
	if !HasCapability(e.webview2RuntimeVersion, SwipeNavigation) {
		return UnsupportedCapabilityError
	}
	webview2Settings, err := e.webview.GetSettings()
	if err != nil {
		return err
	}
	webview2Settings6 := webview2Settings.GetICoreWebView2Settings6()
	err = webview2Settings6.PutIsSwipeNavigationEnabled(enabled)
	if err != nil {
		return err
	}
	return nil
}

func (e *Chromium) PutIsNonClientRegionSupportEnabled(enabled bool) error {
	if !HasCapability(e.webview2RuntimeVersion, NonClientRegion) {
		return UnsupportedCapabilityError
	}
	webview2Settings, err := e.webview.GetSettings()
	if err != nil {
		return err
	}
	webview2Settings9 := webview2Settings.GetICoreWebView2Settings9()
	if webview2Settings9 == nil {
		return UnsupportedCapabilityError
	}
	return webview2Settings9.PutIsNonClientRegionSupportEnabled(enabled)
}

func (e *Chromium) GetNonClientRegionAtPoint(x, y int32) (COREWEBVIEW2_NON_CLIENT_REGION_KIND, bool, error) {
	if !e.NonClientRegionHitTestReady() {
		return COREWEBVIEW2_NON_CLIENT_REGION_KIND_NOWHERE, false, nil
	}

	region, err := e.compositionController4.GetNonClientRegionAtPoint(POINT{X: x, Y: y})
	if err != nil {
		return COREWEBVIEW2_NON_CLIENT_REGION_KIND_NOWHERE, false, err
	}
	return region, true, nil
}

func (e *Chromium) SendMouseInput(
	eventKind COREWEBVIEW2_MOUSE_EVENT_KIND,
	virtualKeys COREWEBVIEW2_MOUSE_EVENT_VIRTUAL_KEYS,
	mouseData uint32,
	x, y int,
) error {
	if !e.CompositionControllerReady() {
		return errors.New("webview2 composition controller is not initialized")
	}

	return e.compositionController.SendMouseInput(eventKind, virtualKeys, mouseData, POINT{X: int32(x), Y: int32(y)})
}

func (e *Chromium) AllowExternalDrag(allow bool) error {
	if !HasCapability(e.webview2RuntimeVersion, AllowExternalDrop) {
		return UnsupportedCapabilityError
	}
	controller := e.GetController()
	controller4 := controller.GetICoreWebView2Controller4()
	err := controller4.PutAllowExternalDrop(allow)
	if err != nil {
		return err
	}
	return nil
}

func (e *Chromium) GetAllowExternalDrag() (bool, error) {
	if !HasCapability(e.webview2RuntimeVersion, AllowExternalDrop) {
		return false, UnsupportedCapabilityError
	}
	controller := e.GetController()
	controller4 := controller.GetICoreWebView2Controller4()
	result, err := controller4.GetAllowExternalDrop()
	if err != nil {
		return false, err
	}
	return result, nil
}

func (e *Chromium) GetCookieManager() (*ICoreWebView2CookieManager, error) {
	if e.webview == nil {
		return nil, errors.New("webview not initialized")
	}

	// Check WebView2 version
	if e.webview2RuntimeVersion == "" {
		return nil, errors.New("WebView2 runtime version not available")
	}

	// Get ICoreWebView2_2 interface
	webview2, err := e.webview.QueryInterface2()
	if err != nil {
		return nil, fmt.Errorf("failed to get ICoreWebView2_2: %w\nThis functionality requires WebView2 Runtime version 89.0.721.0 or later. Current version: %s", err, e.webview2RuntimeVersion)
	}
	defer webview2.Release()

	// Get cookie manager
	cookieManager, err := webview2.GetCookieManager()
	if err != nil {
		return nil, fmt.Errorf("failed to get cookie manager: %w", err)
	}

	// Note: The caller is responsible for calling Release() on the returned cookieManager
	return cookieManager, nil
}
