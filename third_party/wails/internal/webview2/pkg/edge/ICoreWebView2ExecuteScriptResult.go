//go:build windows
// +build windows

package edge

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

type iCoreWebView2ExecuteScriptResultVtbl struct {
	_IUnknownVtbl
	GetSucceeded         ComProc
	GetResultAsJson      ComProc
	TryGetResultAsString ComProc
	GetException         ComProc
}

type iCoreWebView2ExecuteScriptResult struct {
	vtbl *iCoreWebView2ExecuteScriptResultVtbl
}

// AddRef increments the reference count of the iCoreWebView2ExecuteScriptResult interface
func (i *iCoreWebView2ExecuteScriptResult) AddRef() uint32 {
	ret, _, _ := i.vtbl.AddRef.Call(uintptr(unsafe.Pointer(i)))

	return uint32(ret)
}

// Release decrements the reference count of the iCoreWebView2ExecuteScriptResult interface
func (i *iCoreWebView2ExecuteScriptResult) Release() uint32 {
	ret, _, _ := i.vtbl.Release.Call(uintptr(unsafe.Pointer(i)))

	return uint32(ret)
}

func (i *iCoreWebView2ExecuteScriptResult) GetSucceeded() (bool, error) {
	var _value int32

	hr, _, _ := i.vtbl.GetSucceeded.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&_value)),
	)
	if windows.Handle(hr) != windows.S_OK {
		return false, windows.Errno(hr)
	}
	return _value != 0, nil
}

// GetResultAsJson returns the result of the script execution as a JSON string
// (e.g. "1" or "[{...}]" or "null").
func (i *iCoreWebView2ExecuteScriptResult) GetResultAsJson() (string, error) {
	var _jsonResult *uint16

	hr, _, _ := i.vtbl.GetResultAsJson.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(_jsonResult)),
	)
	if windows.Handle(hr) != windows.S_OK {
		return "", windows.Errno(hr)
	}
	jsonResult := windows.UTF16PtrToString(_jsonResult)
	windows.CoTaskMemFree(unsafe.Pointer(_jsonResult))
	return jsonResult, nil
}

// TryGetResultAsString returns the result as a raw string if it is one.
// The bool out-parameter reports whether the result was a string.
func (i *iCoreWebView2ExecuteScriptResult) TryGetResultAsString() (string, bool, error) {
	var _stringResult *uint16
	var _value int32

	hr, _, _ := i.vtbl.TryGetResultAsString.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(_stringResult)),
		uintptr(unsafe.Pointer(&_value)),
	)
	if windows.Handle(hr) != windows.S_OK {
		return "", false, windows.Errno(hr)
	}
	stringResult := windows.UTF16PtrToString(_stringResult)
	windows.CoTaskMemFree(unsafe.Pointer(_stringResult))
	return stringResult, _value != 0, nil
}

func (i *iCoreWebView2ExecuteScriptResult) GetException() (*ICoreWebView2ScriptException, error) {
	var exception *ICoreWebView2ScriptException

	hr, _, _ := i.vtbl.GetException.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&exception)),
	)
	if windows.Handle(hr) != windows.S_OK {
		return nil, windows.Errno(hr)
	}
	return exception, nil
}
