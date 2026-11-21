//go:build windows
// +build windows

package edge

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

type iCoreWebView2ScriptExceptionVtbl struct {
	_IUnknownVtbl
	GetLineNumber   ComProc
	GetColumnNumber ComProc
	GetName         ComProc
	GetMessage      ComProc
	GetToJson       ComProc
}

type ICoreWebView2ScriptException struct {
	vtbl *iCoreWebView2ScriptExceptionVtbl
}

// AddRef increments the reference count of the ICoreWebView2ScriptException interface
func (i *ICoreWebView2ScriptException) AddRef() uint32 {
	ret, _, _ := i.vtbl.AddRef.Call(uintptr(unsafe.Pointer(i)))

	return uint32(ret)
}

// Release decrements the reference count of the ICoreWebView2ScriptException interface
func (i *ICoreWebView2ScriptException) Release() uint32 {
	ret, _, _ := i.vtbl.Release.Call(uintptr(unsafe.Pointer(i)))

	return uint32(ret)
}

func (i *ICoreWebView2ScriptException) GetLineNumber() (uint32, error) {
	var value uint32

	hr, _, _ := i.vtbl.GetLineNumber.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&value)),
	)
	if windows.Handle(hr) != windows.S_OK {
		return 0, windows.Errno(hr)
	}
	return value, nil
}

func (i *ICoreWebView2ScriptException) GetMessage() (string, error) {
	var _value *uint16

	hr, _, _ := i.vtbl.GetMessage.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(_value)),
	)
	if windows.Handle(hr) != windows.S_OK {
		return "", windows.Errno(hr)
	}
	value := windows.UTF16PtrToString(_value)
	windows.CoTaskMemFree(unsafe.Pointer(_value))
	return value, nil
}

func (i *ICoreWebView2ScriptException) GetToJson() (string, error) {
	var _value *uint16

	hr, _, _ := i.vtbl.GetToJson.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(_value)),
	)
	if windows.Handle(hr) != windows.S_OK {
		return "", windows.Errno(hr)
	}
	value := windows.UTF16PtrToString(_value)
	windows.CoTaskMemFree(unsafe.Pointer(_value))
	return value, nil
}
