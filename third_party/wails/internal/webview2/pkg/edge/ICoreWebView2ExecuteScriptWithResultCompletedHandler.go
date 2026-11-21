//go:build windows
// +build windows

package edge

import (
	"unsafe"
)

type iCoreWebView2ExecuteScriptWithResultCompletedHandlerVtbl struct {
	_IUnknownVtbl
	Invoke ComProc
}

type iCoreWebView2ExecuteScriptWithResultCompletedHandler struct {
	vtbl *iCoreWebView2ExecuteScriptWithResultCompletedHandlerVtbl
	impl iCoreWebView2ExecuteScriptWithResultCompletedHandlerImpl
}

func _ICoreWebView2ExecuteScriptWithResultCompletedHandlerIUnknownQueryInterface(this *iCoreWebView2ExecuteScriptWithResultCompletedHandler, refiid, object uintptr) uintptr {
	return this.impl.QueryInterface(refiid, object)
}

func _ICoreWebView2ExecuteScriptWithResultCompletedHandlerIUnknownAddRef(this *iCoreWebView2ExecuteScriptWithResultCompletedHandler) uintptr {
	return handlerAddRef(unsafe.Pointer(this))
}

func _ICoreWebView2ExecuteScriptWithResultCompletedHandlerIUnknownRelease(this *iCoreWebView2ExecuteScriptWithResultCompletedHandler) uintptr {
	return handlerRelease(unsafe.Pointer(this))
}

func iCoreWebView2ExecuteScriptWithResultCompletedHandlerInvoke(this *iCoreWebView2ExecuteScriptWithResultCompletedHandler, errorCode uintptr, result *iCoreWebView2ExecuteScriptResult) uintptr {
	return this.impl.ExecuteScriptWithResultCompleted(errorCode, result)
}

type iCoreWebView2ExecuteScriptWithResultCompletedHandlerImpl interface {
	_IUnknownImpl
	ExecuteScriptWithResultCompleted(errorCode uintptr, result *iCoreWebView2ExecuteScriptResult) uintptr
}

var iCoreWebView2ExecuteScriptWithResultCompletedHandlerFn = iCoreWebView2ExecuteScriptWithResultCompletedHandlerVtbl{
	_IUnknownVtbl{
		NewComProc(_ICoreWebView2ExecuteScriptWithResultCompletedHandlerIUnknownQueryInterface),
		NewComProc(_ICoreWebView2ExecuteScriptWithResultCompletedHandlerIUnknownAddRef),
		NewComProc(_ICoreWebView2ExecuteScriptWithResultCompletedHandlerIUnknownRelease),
	},
	NewComProc(iCoreWebView2ExecuteScriptWithResultCompletedHandlerInvoke),
}

func newICoreWebView2ExecuteScriptWithResultCompletedHandler(impl iCoreWebView2ExecuteScriptWithResultCompletedHandlerImpl) *iCoreWebView2ExecuteScriptWithResultCompletedHandler {
	return &iCoreWebView2ExecuteScriptWithResultCompletedHandler{
		vtbl: &iCoreWebView2ExecuteScriptWithResultCompletedHandlerFn,
		impl: impl,
	}
}
