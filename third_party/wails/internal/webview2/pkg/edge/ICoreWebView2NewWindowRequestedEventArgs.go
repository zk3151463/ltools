//go:build windows

package edge

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// _ICoreWebView2NewWindowRequestedEventArgsVtbl mirrors the ICoreWebView2NewWindowRequestedEventArgs
// COM vtable. We only expose the members the host needs: GetUri (where the page wanted to go)
// and PutHandled (suppress the default "open a new popup window").
type _ICoreWebView2NewWindowRequestedEventArgsVtbl struct {
	_IUnknownVtbl
	// GetUri: out param *PWSTR
	GetUri ComProc
	// PutNewWindow: in *ICoreWebView2 (unused here)
	PutNewWindow ComProc
	// GetNewWindow: out *ICoreWebView2 (unused here)
	GetNewWindow ComProc
	// PutHandled: in BOOL
	PutHandled ComProc
	// GetHandled: out BOOL
	GetHandled ComProc
	// GetIsUserInitiated: out BOOL
	GetIsUserInitiated ComProc
	// GetDeferral: out *ICoreWebView2Deferral (unused here)
	GetDeferral ComProc
	// GetWindowFeatures: out *ICoreWebView2WindowFeatures (unused here)
	GetWindowFeatures ComProc
}

// ICoreWebView2NewWindowRequestedEventArgs is the COM event-args for a
// "new window" request (target="_blank", window.open, <a target> popup).
type ICoreWebView2NewWindowRequestedEventArgs struct {
	vtbl *_ICoreWebView2NewWindowRequestedEventArgsVtbl
}

// GetUri returns the target URI of the new-window request.
func (i *ICoreWebView2NewWindowRequestedEventArgs) GetUri() (string, error) {
	var uri *uint16
	hr, _, _ := i.vtbl.GetUri.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&uri)),
	)
	if windows.Handle(hr) != windows.S_OK {
		return "", windows.Errno(hr)
	}
	s := windows.UTF16PtrToString(uri)
	windows.CoTaskMemFree(unsafe.Pointer(uri))
	return s, nil
}

// PutHandled marks the new-window request as handled. When true (and no
// NewWindow is set), WebView2 opens no popup window.
func (i *ICoreWebView2NewWindowRequestedEventArgs) PutHandled(handled bool) error {
	var v int
	if handled {
		v = 1
	}
	hr, _, _ := i.vtbl.PutHandled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(v),
	)
	if windows.Handle(hr) != windows.S_OK {
		return windows.Errno(hr)
	}
	return nil
}

// GetIsUserInitiated reports whether the request came from a user gesture.
func (i *ICoreWebView2NewWindowRequestedEventArgs) GetIsUserInitiated() (bool, error) {
	var v int32
	hr, _, _ := i.vtbl.GetIsUserInitiated.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&v)),
	)
	if windows.Handle(hr) != windows.S_OK {
		return false, windows.Errno(hr)
	}
	return v != 0, nil
}
