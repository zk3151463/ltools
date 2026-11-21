//go:build windows
// +build windows

package edge

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// iCoreWebView2_20Vtbl is the vtable shape for the ICoreWebView2_20 interface.
//
// ExecuteScriptWithResult lives on ICoreWebView2_21, so we only need the
// embedded ICoreWebView2_2 portion of the vtable to be laid out correctly
// (the slots are identical to iCoreWebView2_2Vtbl); the intermediate
// ICoreWebView2_3.._20 slots are omitted — they are never called through
// this pointer. This mirrors the upstream webview2 package, which likewise
// only declares ICoreWebView2_21 with its embedded ICoreWebView2_20Vtbl.
type iCoreWebView2_20Vtbl struct {
	iCoreWebView2_2Vtbl
}

// iCoreWebView2_21Vtbl adds ExecuteScriptWithResult to the ICoreWebView2_2 vtable.
type iCoreWebView2_21Vtbl struct {
	iCoreWebView2_20Vtbl
	ExecuteScriptWithResult ComProc
}

type ICoreWebView2_21 struct {
	vtbl *iCoreWebView2_21Vtbl
}

// AddRef increments the reference count of the ICoreWebView2_21 interface
func (i *ICoreWebView2_21) AddRef() uint32 {
	ret, _, _ := i.vtbl.AddRef.Call(uintptr(unsafe.Pointer(i)))

	return uint32(ret)
}

// Release decrements the reference count of the ICoreWebView2_21 interface
func (i *ICoreWebView2_21) Release() uint32 {
	ret, _, _ := i.vtbl.Release.Call(uintptr(unsafe.Pointer(i)))

	return uint32(ret)
}

// ExecuteScriptWithResult executes a script and asynchronously returns its
// result via the handler's ExecuteScriptWithResultCompleted callback. The
// script should be an expression whose JSON-serializable value is the result
// (e.g. JSON.stringify(...)); the result is read back with
// ICoreWebView2ExecuteScriptResult.GetResultAsJson.
func (i *ICoreWebView2_21) ExecuteScriptWithResult(javaScript string, handler *iCoreWebView2ExecuteScriptWithResultCompletedHandler) error {
	_javaScript, err := windows.UTF16PtrFromString(javaScript)
	if err != nil {
		return err
	}

	hr, _, _ := i.vtbl.ExecuteScriptWithResult.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(_javaScript)),
		uintptr(unsafe.Pointer(handler)),
	)
	if windows.Handle(hr) != windows.S_OK {
		return windows.Errno(hr)
	}

	return nil
}

// GetICoreWebView2_21 queries the ICoreWebView2_21 interface
// (IID {c4980dea-587b-43b9-8143-3ef3bf552d95}) from the base ICoreWebView2.
// Returns nil if the runtime does not support ICoreWebView2_21.
// The caller owns one reference and must Release it when done.
func (i *ICoreWebView2) GetICoreWebView2_21() *ICoreWebView2_21 {
	var result *ICoreWebView2_21

	iidICoreWebView2_21 := NewGUID("{c4980dea-587b-43b9-8143-3ef3bf552d95}")
	hr, _, _ := i.vtbl.QueryInterface.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(iidICoreWebView2_21)),
		uintptr(unsafe.Pointer(&result)))
	if windows.Handle(hr) != windows.S_OK {
		return nil
	}

	return result
}
