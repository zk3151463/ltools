package application

import (
	"time"
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/events"
)

type Window interface {
	Center()
	Close()
	DisableSizeConstraints()
	DispatchWailsEvent(event *CustomEvent)
	EmitEvent(name string, data ...any) bool
	EnableSizeConstraints()
	Error(message string, args ...any)
	ExecJS(js string)
	// ExecJSResult executes js and synchronously returns its result (the
	// JSON-serialised value of the expression), blocking up to timeout.
	// On Windows this uses the WebView2 COM callback
	// (ICoreWebView2_21.ExecuteScriptWithResult); a non-nil error means
	// "no data" (JS error / timeout / unsupported runtime / no-op platform).
	ExecJSResult(js string, timeout time.Duration) (string, error)
	Focus()
	ForceReload()
	Fullscreen() Window
	GetBorderSizes() *LRTB
	GetScreen() (*Screen, error)
	GetZoom() float64
	handleDragAndDropMessage(filenames []string, dropTarget *DropTargetDetails)
	InitiateFrontendDropProcessing(filenames []string, x int, y int)
	HandleMessage(message string)
	HandleWindowEvent(id uint)
	Height() int
	Hide() Window
	HideMenuBar()
	ID() uint
	Info(message string, args ...any)
	IsFocused() bool
	IsFullscreen() bool
	IsIgnoreMouseEvents() bool
	IsMaximised() bool
	IsMinimised() bool
	HandleKeyEvent(acceleratorString string)
	Maximise() Window
	Minimise() Window
	Name() string
	OnWindowEvent(eventType events.WindowEventType, callback func(event *WindowEvent)) func()
	OpenContextMenu(data *ContextMenuData)
	Position() (int, int)
	RelativePosition() (int, int)
	Reload()
	Resizable() bool
	Restore()
	Run()
	SetPosition(x, y int)
	SetAlwaysOnTop(b bool) Window
	SetBackgroundColour(colour RGBA) Window
	SetFrameless(frameless bool) Window
	SetHTML(html string) Window
	SetMinimiseButtonState(state ButtonState) Window
	SetMaximiseButtonState(state ButtonState) Window
	SetCloseButtonState(state ButtonState) Window
	SetFullscreenButtonState(state ButtonState) Window
	SetMaxSize(maxWidth, maxHeight int) Window
	SetMinSize(minWidth, minHeight int) Window
	SetRelativePosition(x, y int) Window
	SetScreen(screen *Screen) Window
	SetResizable(b bool) Window
	SetIgnoreMouseEvents(ignore bool) Window
	SetSize(width, height int) Window
	SetTitle(title string) Window
	SetURL(s string) Window
	SetZoom(magnification float64) Window
	Show() Window
	ShowMenuBar()
	Size() (width int, height int)
	OpenDevTools()
	ToggleFullscreen()
	ToggleMaximise()
	ToggleMenuBar()
	ToggleFrameless()
	UnFullscreen()
	UnMaximise()
	UnMinimise()
	Width() int
	IsVisible() bool
	Bounds() Rect
	SetBounds(bounds Rect)
	Zoom()
	ZoomIn()
	ZoomOut()
	ZoomReset() Window
	SetMenu(menu *Menu)
	SnapAssist()
	SetContentProtection(protection bool) Window
	NativeWindow() unsafe.Pointer
	SetEnabled(enabled bool)
	Flash(enabled bool)
	Print() error
	RegisterHook(eventType events.WindowEventType, callback func(event *WindowEvent)) func()
	AttachModal(modalWindow Window)
	shouldUnconditionallyClose() bool

	// Editing methods
	cut()
	copy()
	paste()
	undo()
	redo()
	delete()
	selectAll()
}
