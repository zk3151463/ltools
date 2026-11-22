package main

import (
	"embed"
	_ "embed"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"ltools/internal/plugins"
	"ltools/internal/proxy"
	"ltools/internal/settings"
	"ltools/internal/sync"
	"ltools/internal/update"
	"ltools/plugins/col1688"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// Wails uses Go's `embed` package to embed the frontend files into the binary.
// Any files in the frontend/dist folder will be embedded into the binary and
// made available to the frontend.
// See https://pkg.go.dev/embed for more information.

//go:embed all:frontend/dist
var assets embed.FS

//go:embed build/appicon.png
var systrayIcon []byte

// 版本号，通过 -ldflags 在编译时注入
// 示例: go build -ldflags="-X main.version=0.1.2"
var version = "0.1.0" // 默认值，开发时使用

func init() {
	// Register custom events for the shortcut service
	application.RegisterEvent[string]("shortcut:triggered")

	// Register event for permission requirement (sends map with title, message, platform)
	// We need to define the event type - using map[string]interface{} for flexibility
	// Note: Wails v3 events need a specific type, so we'll use string for the data
	application.RegisterEvent[string]("shortcut:permission-required")

	// Register notification event for user notifications
	application.RegisterEvent[string]("notification:show")

	// Register custom events for the search window service
	application.RegisterEvent[string]("search:opened")
	application.RegisterEvent[string]("search:closed")
	application.RegisterEvent[[]*plugins.SearchResult]("search:results")

	// Register custom events for the update service
	application.RegisterEvent[*update.UpdateInfo]("update:available")
	application.RegisterEvent[int]("update:progress")

	// Register custom event for file open (file association)
	application.RegisterEvent[string]("file:open")

	// Register custom event for URL open (custom protocol)
	application.RegisterEvent[map[string]string]("url:open")

	// Register custom events for the 1688 collector plugin
	application.RegisterEvent[col1688.LogEntry]("c1688:log")
	application.RegisterEvent[[]col1688.Supplier]("c1688:result")
	application.RegisterEvent[map[string]int]("c1688:progress")
	// 单条供应商（逐条累积，前端结果表实时加行）
	application.RegisterEvent[col1688.Supplier]("c1688:item")
	application.RegisterEvent[string]("c1688:state")
	// 1688 子窗口回传当前页 HTML（Wails 事件总线，Go 端监听后 parseHTML）
	application.RegisterEvent[map[string]string]("c1688:page-html")
}

// main function serves as the application's entry point. It initializes the application, creates a window,
// registers plugins, and runs the application.
func main() {

	// Create proxy manager for all plugins
	proxyManager := proxy.NewProxyManager(&proxy.ProxyConfig{
		RequestTimeout:  30 * time.Second,
		MaxRetries:      2,
		EnableCache:     true,
		CacheTTL:        30 * time.Minute,
		MaxCacheEntries: 1000,
		EnableLogging:   true,
	})

	// 创建组合资源处理器：代理请求 + 静态文件
	proxyHandler := NewCombinedAssetHandler(proxyManager, application.AssetFileServerFS(assets))

	// Create a new Wails application by providing the necessary options.
	// Variables 'Name' and 'Description' are for application metadata.
	// 'Assets' configures the asset server with the 'FS' variable pointing to the frontend files.
	// 'Services' is a list of Go struct instances. The frontend has access to the methods of these instances.
	// 'Mac' options tailor the application when running an macOS.
	app := application.New(application.Options{
		Name:        "ltools",
		Description: "A plugin-based desktop toolbox",
		Assets: application.AssetOptions{
			Handler: proxyHandler,
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: false, // 保持应用在窗口关闭后运行
		},
		// 参考 https://learn.microsoft.com/en-us/microsoft-edge/webview2/concepts/webview-features-flags?tabs=dotnetcsharp
		Windows: application.WindowsOptions{
			EnabledFeatures: []string{
				"ignore-certificate-errors",      // 忽略与证书相关的错误
				"allow-insecure-localhost",       // 允许忽略本地主机上的 TLS/SSL 错误
				"allow-running-insecure-content", // 解除对从 HTTP 源提供内容的 MSP 的阻止
				"autoplay-policy",                // 允许自动播放策略
			},
			DisabledFeatures: []string{},
			AdditionalBrowserArgs: []string{
				"--disable-web-security", // 禁用同源策略
			}, // webview2 启动参数
		},
		// 子窗口（外部 1688 页）用 window.external.invoke 回传 c1688:page-html payload。
		// Wails 框架层 handleWindowMessage 对非 "wails:" 开头消息调此回调 →
		// col1688.DispatchRawMessage → 各 engine 的 handleRawMessage → pageHTMLCh。
		// WebView2 原生通信，不被 CSP 拦，不走 127.0.0.1。
		RawMessageHandler: func(window application.Window, message string, originInfo *application.OriginInfo) {
			log.Printf("[Main] RawMessageHandler called（len=%d, origin=%v）", len(message), originInfo)
			col1688.DispatchRawMessage(message)
		},
	})

	// 设置 proxyHandler 的 app 引用，用于访问环境信息
	proxyHandler.SetApp(app)

	// 监听文件打开事件（文件关联）
	app.Event.OnApplicationEvent(events.Common.ApplicationOpenedWithFile, func(e *application.ApplicationEvent) {
		filePath := e.Context().Filename()
		log.Printf("[Main] File opened via association: %s", filePath)
		// 发送事件到前端，让前端处理文件打开
		app.Event.Emit("file:open", filePath)
	})

	// 注意：URL 打开事件监听已移至 SearchWindowService 创建之后
	// 见下方 "Set the main window reference in SearchWindowService" 之后

	// 创建系统托盘
	systray := app.SystemTray.New()
	systray.SetIcon(systrayIcon)
	systray.SetLabel("LTools")

	// 保存主窗口引用，用于系统托盘菜单
	var mainWindow *application.WebviewWindow

	// 创建系统托盘菜单
	menu := app.NewMenu()
	menu.Add("显示窗口").OnClick(func(_ *application.Context) {
		if mainWindow != nil {
			mainWindow.Show()
		}
	})
	menu.Add("隐藏窗口").OnClick(func(_ *application.Context) {
		if mainWindow != nil {
			mainWindow.Hide()
		}
	})
	menu.AddSeparator()
	menu.Add("关于 LTools").OnClick(func(_ *application.Context) {
		if mainWindow != nil {
			// 显示并聚焦窗口
			mainWindow.Show()
			mainWindow.Focus()
			// 通过事件通知前端导航到关于页面，避免页面重新加载
			app.Event.Emit("navigate:to", map[string]string{
				"path": "/settings",
				"tab":  "about",
			})
		}
	})
	menu.AddSeparator()
	menu.Add("退出").OnClick(func(_ *application.Context) {
		app.Quit()
	})

	systray.SetMenu(menu)

	// Get user data directory for plugin storage
	userDataDir, err := os.UserConfigDir()
	if err != nil {
		log.Fatal("Failed to get user config dir:", err)
	}
	dataDir := filepath.Join(userDataDir, "ltools")

	// Create plugin manager
	pluginManager, err := plugins.NewManager(app, dataDir)
	if err != nil {
		log.Fatal("Failed to create plugin manager:", err)
	}

	// Register the 1688 collector plugin (name/phone/address of factories & suppliers)
	col1688Plugin := col1688.NewPlugin()
	if err := col1688Plugin.Init(app); err != nil {
		log.Printf("[Main] Failed to init col1688 plugin: %v", err)
	}
	if err := col1688Plugin.SetDataDir(dataDir); err != nil {
		log.Printf("[Main] Failed to set data dir for col1688: %v", err)
	}
	if err := pluginManager.Register(col1688Plugin); err != nil {
		log.Printf("[Main] Failed to register col1688 plugin: %v", err)
	}

	// Start all enabled plugins - this calls ServiceStartup() on each enabled plugin
	// This is crucial for plugins like clipboard that need to start background monitoring
	if err := pluginManager.StartupAll(); err != nil {
		log.Fatal("Failed to startup plugins:", err)
	}

	// Create plugin service to expose plugin management to frontend
	pluginService := plugins.NewPluginService(pluginManager, app)
	if err != nil {
		log.Fatal("Failed to create shortcut service:", err)
	}

	// Create and register the 1688 collector service for the frontend.
	// 复用插件持有的**唯一**服务实例（SetDataDir 已惰性创建），
	// 不再 new 第三个 → 避免多实例各自 registerEvents 导致
	// 多个 Event.On("c1688:page-html") 监听器 + 各自独立 channel 的竞争。
	col1688Service := col1688Plugin.Service()

	// Create sync service for Git-based data synchronization
	syncService, err := sync.NewSyncService(app, dataDir)
	if err != nil {
		log.Fatal("Failed to create sync service:", err)
	}

	// Create settings service for general app settings
	settingsService := settings.NewService()

	// Create shortcut service to expose keyboard shortcut management to frontend
	shortcutService, err := plugins.NewShortcutService(app, dataDir)
	if err != nil {
		log.Fatal("Failed to create shortcut service:", err)
	}

	// Create search window service for global search functionality
	searchWindowService := plugins.NewSearchWindowService(app, pluginService, shortcutService)

	// Create update service
	// Note: version is injected at build time via -ldflags
	// Example: go build -ldflags="-X main.version=0.1.2"
	updateService := update.NewService(&update.ServiceConfig{
		CurrentVersion: version,
		UpdateURL:      "https://raw.githubusercontent.com/lian-yang/ltools/main/",
		DataDir:        dataDir,
		Enabled:        true, // TODO: 从用户配置读取
	}, app)

	// Register services
	app.RegisterService(application.NewService(pluginService))
	app.RegisterService(application.NewService(shortcutService))
	app.RegisterService(application.NewService(searchWindowService))
	app.RegisterService(application.NewService(syncService))
	app.RegisterService(application.NewService(settingsService))
	app.RegisterService(application.NewService(col1688Service))

	// Start sync service
	if err := syncService.ServiceStartup(app); err != nil {
		log.Printf("Failed to start sync service: %v", err)
	}
	app.RegisterService(application.NewService(updateService))

	// Create a new window with the necessary options.
	// 'Title' is the title of the window.
	// 'Mac' options tailor the window when running on macOS.
	// 'BackgroundColour' is the background colour of the window.
	// 'URL' is the URL that will be loaded into the webview.
	mainWindow = app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:     "LTools",
		Width:     1400,
		Height:    900,
		MinWidth:  1400,
		MinHeight: 900,
		//DisableResize:       true,                      // 禁用调整大小
		MinimiseButtonState: application.ButtonEnabled, // 开启最小化按钮
		MaximiseButtonState: application.ButtonEnabled, // 开启最大化按钮
		UseApplicationMenu:  true,                      // 使用应用程序菜单（对 macOS 无影响）
		Mac: application.MacWindow{
			TitleBar: application.MacTitleBar{
				HideTitle:          true, // 隐藏标题文字
				AppearsTransparent: true, // 标题栏透明
				FullSizeContent:    true, // 内容延伸到标题栏区域（关键！）
			},
			Backdrop:                application.MacBackdropTransparent,
			InvisibleTitleBarHeight: 40, // 不可见标题栏区域（用于拖动）的高度
		},
		BackgroundColour: application.NewRGB(27, 38, 54),
		URL:              "/",
		EnableFileDrop:   true, // 启用文件拖放
		Windows: application.WindowsWindow{
			Permissions: map[application.CoreWebView2PermissionKind]application.CoreWebView2PermissionState{
				application.CoreWebView2PermissionKindCamera:        application.CoreWebView2PermissionStateAllow, // 相机权限
				application.CoreWebView2PermissionKindMicrophone:    application.CoreWebView2PermissionStateAllow, // 麦克风权限
				application.CoreWebView2PermissionKindNotifications: application.CoreWebView2PermissionStateAllow, // 通知权限
			},
		},
	})

	// 监听主窗口关闭事件，隐藏窗口而非销毁（系统托盘应用模式）
	mainWindow.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		mainWindow.Hide()
		e.Cancel() // 阻止窗口真正关闭
	})

	// Start the search window service
	searchWindowService.ServiceStartup(app)

	// Start the shortcut service AFTER window is created
	// This is crucial for Wails v3 KeyBinding API to work properly
	if err := shortcutService.ServiceStartup(app); err != nil {
		log.Printf("Failed to start shortcut service: %v", err)
	}

	// Set the main window reference in ShortcutService so it can focus the window
	shortcutService.SetMainWindow(mainWindow)

	// Set the main window reference in SearchWindowService so it can show the main window
	searchWindowService.SetMainWindow(mainWindow)

	// 监听 URL 打开事件（自定义协议 ltools://）
	// 注意：此监听必须在 SearchWindowService 和 mainWindow 创建之后设置
	app.Event.OnApplicationEvent(events.Common.ApplicationLaunchedWithUrl, func(e *application.ApplicationEvent) {
		url := e.Context().URL()
		log.Printf("[Main] URL opened via custom protocol: %s", url)

		// 解析 ltools:// 协议 URL
		// 支持格式:
		// - ltools://settings?tab=about
		// - ltools://plugins/{pluginId}?param=xxx
		// - ltools://search?q=keyword
		if len(url) > 9 && url[:9] == "ltools://" {
			path := url[9:] // 移除 "ltools://" 前缀
			log.Printf("[Main] Parsed ltools URL path: %s", path)

			// 对于非搜索 URL，先确保主窗口获得焦点
			if !strings.HasPrefix(path, "search") {
				// 如果搜索窗口可见，先隐藏它
				if searchWindowService.IsVisible() {
					log.Printf("[Main] Hiding search window before opening URL")
					searchWindowService.Hide()
				}
				// 显示并聚焦主窗口
				if mainWindow != nil {
					log.Printf("[Main] Showing and focusing main window")
					mainWindow.Show()
					mainWindow.Focus()
				}
			}

			// 发送事件到前端，让前端处理导航
			app.Event.Emit("url:open", map[string]string{
				"url":  url,
				"path": path,
			})
		}
	})

	// Register default search window hotkey (Cmd+5 / Ctrl+5)
	// Using Cmd+5 which might work better with gohook
	// Note: This will override the sysinfo plugin's shortcut
	searchHotkey := "cmd+5" // macOS
	if runtime.GOOS != "darwin" {
		searchHotkey = "ctrl+5" // Windows/Linux
	}

	// Remove old search window shortcuts if they exist
	// Also remove cmd+5 which is currently used by sysinfo plugin
	oldShortcuts := []string{"cmd+space", "ctrl+space", "cmd+shift+p", "ctrl+shift+p", "alt+space", "cmd+k", "ctrl+k", "cmd+9", "ctrl+9", "cmd+l", "ctrl+l", "cmd+6", "ctrl+6", "cmd+5", "ctrl+5"}
	for _, oldShortcut := range oldShortcuts {
		if err := shortcutService.RemoveShortcut(oldShortcut); err != nil {
			// Ignore errors - the shortcut might not exist
			log.Printf("[Main] Note: %s (may not exist): %v", oldShortcut, err)
		}
	}

	// Try to set the default search shortcut
	// If it conflicts, we'll let the user know
	if err := shortcutService.SetShortcut(searchHotkey, "search.window.builtin"); err != nil {
		log.Printf("[Main] Failed to set default search hotkey %s: %v (may conflict with existing shortcut)", searchHotkey, err)
	} else {
		log.Printf("[Main] Default search hotkey registered: %s", searchHotkey)
	}

	// Check for updates in background (delayed 10 seconds to not block startup)
	go func() {
		time.Sleep(10 * time.Second)
		if updateService.IsEnabled() {
			info, err := updateService.CheckForUpdate()
			if err != nil {
				log.Printf("[Main] Failed to check for updates: %v", err)
				return
			}
			if info != nil {
				log.Printf("[Main] Update available: %s", info.Version)
				// Emit event to frontend
				app.Event.Emit("update:available", info)
			}
		}
	}()

	// Run the application. This blocks until the application has been exited.
	err = app.Run()

	// If an error occurred while running the application, log it and exit.
	if err != nil {
		log.Fatal(err)
	}
}
