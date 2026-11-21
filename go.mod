module ltools

go 1.26.0

require (
	github.com/robotn/gohook v0.42.3
	github.com/wailsapp/wails/v3 v3.1.0
	github.com/zalando/go-keyring v0.2.6
	golang.org/x/sys v0.48.0
	howett.net/plist v1.0.2-0.20250314012144-ee69052608d9
)

// Local vendored copy of Wails v3 with a minimal NewWindowRequested patch
// (edge.Chromium wiring + per-window OnNewWindowRequest option). Remove this
// once upstream ships OnNewWindowRequest / OnBeforeNavigate (PR #6300).
replace github.com/wailsapp/wails/v3 => ./third_party/wails

require (
	al.essio.dev/pkg/shellescape v1.6.0 // indirect
	github.com/coder/websocket v1.8.14 // indirect
	github.com/danieljoos/wincred v1.2.3 // indirect
	github.com/ebitengine/purego v0.11.1 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/vcaesar/keycode v0.10.1 // indirect
	golang.org/x/term v0.46.0 // indirect
)
