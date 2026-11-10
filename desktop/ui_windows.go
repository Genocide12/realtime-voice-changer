//go:build windows

package main

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/jchv/go-webview2/pkg/edge"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassExW  = user32.NewProc("RegisterClassExW")
	procCreateWindowExW   = user32.NewProc("CreateWindowExW")
	procDefWindowProcW    = user32.NewProc("DefWindowProcW")
	procShowWindow        = user32.NewProc("ShowWindow")
	procDestroyWindow     = user32.NewProc("DestroyWindow")
	procGetMessageW       = user32.NewProc("GetMessageW")
	procTranslateMessage  = user32.NewProc("TranslateMessage")
	procDispatchMessageW  = user32.NewProc("DispatchMessageW")
	procPostQuitMessage   = user32.NewProc("PostQuitMessage")
	procMessageBoxW       = user32.NewProc("MessageBoxW")
	procLoadIconW         = user32.NewProc("LoadIconW")
	procGetSystemMetrics  = user32.NewProc("GetSystemMetrics")
	procGetWindowRect     = user32.NewProc("GetWindowRect")
	procSetWindowPos      = user32.NewProc("SetWindowPos")
	procGetModuleHandleW  = kernel32.NewProc("GetModuleHandleW")
	procSetProcessDPIAware = user32.NewProc("SetProcessDPIAware")
)

type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type winMsg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

type rect struct{ L, T, R, B int32 }

const (
	wsOverlappedWindow = 0x00CF0000
	wmSize             = 0x05
	wmMove             = 0x03
	wmClose            = 0x10
	wmDestroy          = 0x02
	swShow             = 5
	mbIconError        = 0x10
	hwndTop            = 0
	swpNoZOrder        = 0x0004
)

var cwUseDefault = uintptr(uint32(0x80000000)) // CW_USEDEFAULT

var (
	appChromium *edge.Chromium
	appURL      string
)

func fatal(text string) {
	t, _ := windows.UTF16PtrFromString("Голос-подменщик")
	m, _ := windows.UTF16PtrFromString(text)
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), mbIconError)
	os.Exit(1)
}

func wndProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch msg {
	case wmMove:
		if appChromium != nil {
			_ = appChromium.NotifyParentWindowPositionChanged()
		}
	case wmSize:
		if appChromium != nil {
			appChromium.Resize()
		}
	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

func centerWindow(hwnd uintptr) {
	var r rect
	procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	swp, _, _ := procGetSystemMetrics.Call(0)
	shp, _, _ := procGetSystemMetrics.Call(1)
	sw := int(int32(swp))
	sh := int(int32(shp))
	ww := int(r.R) - int(r.L)
	wh := int(r.B) - int(r.T)
	x, y := 0, 0
	if sw > ww {
		x = (sw - ww) / 2
	}
	if sh > wh {
		y = (sh - wh) / 2
	}
	procSetWindowPos.Call(hwnd, hwndTop, uintptr(int32(x)), uintptr(int32(y)), 0, 0, swpNoZOrder|0x0001)
}

func runUI(url string) {
	appURL = url
	_, _, _ = procSetProcessDPIAware.Call()

	hInst, _, _ := procGetModuleHandleW.Call(0)
	className, _ := windows.UTF16PtrFromString("VoiceChangerWnd")
	title, _ := windows.UTF16PtrFromString("Голос-подменщик")

	// иконка из ресурса (если вшита .syso), иначе стандартная
	icon, _, _ := procLoadIconW.Call(hInst, 1)
	if icon == 0 {
		icon, _, _ = procLoadIconW.Call(0, 32512)
	}

	wc := wndClassEx{
		CbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		LpfnWndProc:   windows.NewCallback(wndProc),
		HInstance:     hInst,
		HIcon:         icon,
		HIconSm:       icon,
		LpszClassName: className,
	}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	hwnd, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		wsOverlappedWindow,
		cwUseDefault, cwUseDefault, 1240, 860,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		fatal("Не удалось создать окно приложения.")
	}

	// WebView2: нативное окно без браузера, микрофон разрешаем сразу
	chromium := edge.NewChromium()
	chromium.DataPath = userDataPath()
	chromium.SetGlobalPermission(edge.CoreWebView2PermissionStateAllow)
	appChromium = chromium

	if !chromium.Embed(hwnd) {
		// Нет WebView2 Runtime — запасной вариант: системный браузер.
		appChromium = nil
		openBrowser(url)
		fmt.Println("WebView2 недоступен, открыто в браузере:", url)
		waitInterrupt()
		return
	}
	settings, err := chromium.GetSettings()
	if err == nil {
		_ = settings.PutAreDefaultContextMenusEnabled(false)
		_ = settings.PutAreDevToolsEnabled(false)
	}
	procShowWindow.Call(hwnd, swShow)
	centerWindow(hwnd)
	chromium.Resize()
	chromium.Navigate(url)

	var m winMsg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		code := int32(r)
		if code == 0 || code == -1 {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func userDataPath() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.Getenv("APPDATA")
	}
	if base == "" {
		base = os.TempDir()
	}
	return base + "\\VoiceChanger"
}
