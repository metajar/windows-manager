//go:build windows

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	pRegisterClassExW  = user32.NewProc("RegisterClassExW")
	pCreateWindowExW   = user32.NewProc("CreateWindowExW")
	pDefWindowProcW    = user32.NewProc("DefWindowProcW")
	pShowWindow        = user32.NewProc("ShowWindow")
	pSetWindowPos      = user32.NewProc("SetWindowPos")
	pGetMessageW       = user32.NewProc("GetMessageW")
	pTranslateMessage  = user32.NewProc("TranslateMessage")
	pDispatchMessageW  = user32.NewProc("DispatchMessageW")
	pBeginPaint        = user32.NewProc("BeginPaint")
	pEndPaint          = user32.NewProc("EndPaint")
	pFillRect          = user32.NewProc("FillRect")
	pDrawTextW         = user32.NewProc("DrawTextW")
	pGetClientRect     = user32.NewProc("GetClientRect")
	pInvalidateRect    = user32.NewProc("InvalidateRect")
	pSetForegroundWin  = user32.NewProc("SetForegroundWindow")
	pSetTimer          = user32.NewProc("SetTimer")
	pGetSystemMetrics  = user32.NewProc("GetSystemMetrics")
	pLoadCursorW       = user32.NewProc("LoadCursorW")
	pSetWindowsHookExW = user32.NewProc("SetWindowsHookExW")
	pCallNextHookEx    = user32.NewProc("CallNextHookEx")

	pCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")
	pSetTextColor     = gdi32.NewProc("SetTextColor")
	pSetBkMode        = gdi32.NewProc("SetBkMode")
	pCreateFontW      = gdi32.NewProc("CreateFontW")
	pSelectObject     = gdi32.NewProc("SelectObject")

	pGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
)

const (
	wsPopup        = 0x80000000
	wsExTopmost    = 0x00000008
	wsExToolWindow = 0x00000080

	swHide = 0
	swShow = 5

	swpShowWindow = 0x0040

	smXVirtual  = 76
	smYVirtual  = 77
	smCXVirtual = 78
	smCYVirtual = 79

	wmDestroy = 0x0002
	wmPaint   = 0x000F
	wmTimer   = 0x0113
	wmChar    = 0x0102

	dtCenter     = 0x0001
	dtVCenter    = 0x0004
	dtSingleLine = 0x0020
	dtNoPrefix   = 0x0800
	dtWordBreak  = 0x0010

	bkTransparent = 1

	whKeyboardLL = 13
	hcAction     = 0
	llkhfAltDown = 0x20

	vkTab    = 0x09
	vkEscape = 0x1B
	vkLWin   = 0x5B
	vkRWin   = 0x5C
	vkF4     = 0x73
)

// HWND_TOPMOST is (HWND)-1.
var hwndTopmost = ^uintptr(0)

type point struct{ x, y int32 }
type rect struct{ left, top, right, bottom int32 }

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

type paintStruct struct {
	hdc         uintptr
	fErase      int32
	rcPaint     rect
	fRestore    int32
	fIncUpdate  int32
	rgbReserved [32]byte
}

type kbdLLHookStruct struct {
	vkCode      uint32
	scanCode    uint32
	flags       uint32
	time        uint32
	dwExtraInfo uintptr
}

// PIN verification states. The submit round trip (server or pipe) can take
// seconds, so it must never run on the message-loop thread; these atomics
// carry the state between the worker goroutine and the painter.
const (
	pinIdle     int32 = iota // normal prompt
	pinChecking              // attempt in flight
	pinWrong                 // last attempt rejected
)

// Package-level UI state. Only the message-loop thread touches hwnd/pinBuf/
// visible; the controller and pinState are read via atomics from the loop,
// the hook, and the PIN worker goroutine.
var (
	gCtrl    *LockController
	gHWND    uintptr
	gVisible bool
	pinBuf   []rune

	pinState     atomic.Int32
	lastPinState int32 // loop-thread copy, to detect repaint-worthy changes

	fontBig   uintptr
	fontMid   uintptr
	fontSmall uintptr
	bgBrush   uintptr

	vx, vy, vw, vh int32
)

func u16(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

// runOverlay creates the (hidden) fullscreen window, installs the keyboard
// hook, and runs the Win32 message loop on a dedicated OS thread. It returns
// only when the process is shutting down.
func runOverlay(ctx context.Context, ctrl *LockController) {
	gCtrl = ctrl

	// Win32 windows are thread-affine: the window must be created, hooked, and
	// pumped from the same OS thread. Without this pin the Go scheduler migrates
	// the goroutine between threads and the window stops receiving messages
	// entirely — it shows as "Not Responding", keystrokes go nowhere, and the
	// WM_TIMER that hides the overlay after a grant never fires.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hInst, _, _ := pGetModuleHandleW.Call(0)
	cursor, _, _ := pLoadCursorW.Call(0, 32512) // IDC_ARROW
	bgBrush, _, _ = pCreateSolidBrush.Call(0x000A0A0A)

	className := u16("rewarddLock")
	wc := wndClassExW{
		style:         0x0003, // CS_HREDRAW|CS_VREDRAW
		lpfnWndProc:   syscall.NewCallback(wndProc),
		hInstance:     hInst,
		hCursor:       cursor,
		hbrBackground: bgBrush,
		lpszClassName: className,
	}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	if ret, _, err := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); ret == 0 {
		log.Fatalf("RegisterClassExW: %v", err)
	}

	vx = int32(metric(smXVirtual))
	vy = int32(metric(smYVirtual))
	vw = int32(metric(smCXVirtual))
	vh = int32(metric(smCYVirtual))

	hwnd, _, err := pCreateWindowExW.Call(
		wsExTopmost|wsExToolWindow,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(u16("rewardd"))),
		wsPopup,
		uintptr(vx), uintptr(vy), uintptr(vw), uintptr(vh),
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		log.Fatalf("CreateWindowExW: %v", err)
	}
	gHWND = hwnd

	fontBig = makeFont(-72, 800)
	fontMid = makeFont(-34, 600)
	fontSmall = makeFont(-22, 400)

	// 250ms tick drives show/hide, topmost re-assertion, and repaint when the
	// controller's lock/remaining state changes underneath us (e.g. a parent
	// grants time and the next heartbeat clears the lock).
	pSetTimer.Call(hwnd, 1, 250, 0)

	// Install the low-level keyboard hook on this thread.
	if h, _, herr := pSetWindowsHookExW.Call(whKeyboardLL, syscall.NewCallback(hookProc), hInst, 0); h == 0 {
		log.Printf("warning: keyboard hook not installed: %v", herr)
	}

	// Exit the loop if the context is cancelled (dev/testing convenience).
	go func() {
		<-ctx.Done()
		os.Exit(0)
	}()

	var m msg
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 { // 0 = WM_QUIT, -1 = error
			return
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func metric(i int) int {
	r, _, _ := pGetSystemMetrics.Call(uintptr(i))
	return int(int32(r))
}

func makeFont(height, weight int32) uintptr {
	f, _, _ := pCreateFontW.Call(
		uintptr(height), 0, 0, 0, uintptr(weight),
		0, 0, 0,
		1, 0, 0, 0, 0, // DEFAULT_CHARSET
		uintptr(unsafe.Pointer(u16("Segoe UI"))),
	)
	return f
}

func wndProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmTimer:
		tick(hwnd)
		return 0
	case wmChar:
		handleChar(hwnd, rune(wParam))
		return 0
	case wmPaint:
		paint(hwnd)
		return 0
	case wmDestroy:
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, uintptr(message), wParam, lParam)
	return r
}

func tick(hwnd uintptr) {
	locked := gCtrl.Locked()
	if locked {
		if !gVisible {
			// Locked transition: show, raise to top, and grab focus exactly
			// once. Re-running SetForegroundWindow on every 250ms tick (the old
			// behavior) fights the user's own clicks and makes Windows mark the
			// window "Not Responding"; we only need to assert it on show.
			pShowWindow.Call(hwnd, swShow)
			gVisible = true
			pSetWindowPos.Call(hwnd, hwndTopmost,
				uintptr(vx), uintptr(vy), uintptr(vw), uintptr(vh), swpShowWindow)
			pSetForegroundWin.Call(hwnd)
			pinBuf = pinBuf[:0]
			pinState.Store(pinIdle)
			lastPinState = pinIdle
			pInvalidateRect.Call(hwnd, 0, 1)
		} else if ps := pinState.Load(); ps != lastPinState {
			// PIN worker reported a result: repaint the prompt.
			lastPinState = ps
			pInvalidateRect.Call(hwnd, 0, 1)
		}
	} else if gVisible {
		pShowWindow.Call(hwnd, swHide)
		gVisible = false
		pinBuf = pinBuf[:0]
		pinState.Store(pinIdle)
		lastPinState = pinIdle
	}
}

func handleChar(hwnd uintptr, ch rune) {
	if !gCtrl.Locked() {
		return
	}
	// Ignore typing while a previous attempt is still being checked.
	if pinState.Load() == pinChecking {
		return
	}
	switch ch {
	case '\r', '\n': // Enter: submit
		pin := string(pinBuf)
		pinBuf = pinBuf[:0]
		if pin != "" {
			submitPINAsync(pin)
		}
		pInvalidateRect.Call(hwnd, 0, 1)
	case '\b': // Backspace
		if len(pinBuf) > 0 {
			pinBuf = pinBuf[:len(pinBuf)-1]
		}
		pInvalidateRect.Call(hwnd, 0, 1)
	default:
		if ch >= ' ' && len(pinBuf) < 16 {
			pinBuf = append(pinBuf, ch)
			pinState.Store(pinIdle) // clear any prior "wrong" message as they retype
			pInvalidateRect.Call(hwnd, 0, 1)
		}
	}
}

// submitPINAsync validates a PIN off the message-loop thread. SubmitPIN can
// block on a network round trip (server unlock) or a named-pipe exchange (face
// mode); running it inline would freeze the message pump and wedge the overlay.
// The result is published via pinState and picked up by the next tick().
func submitPINAsync(pin string) {
	pinState.Store(pinChecking)
	go func() {
		ok := gCtrl.SubmitPIN(pin)
		if ok {
			// tick() sees Locked()==false and hides the overlay.
			pinState.Store(pinIdle)
			return
		}
		pinState.Store(pinWrong)
	}()
}

func paint(hwnd uintptr) {
	var ps paintStruct
	hdc, _, _ := pBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))

	var rc rect
	pGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	pFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), bgBrush)

	pSetBkMode.Call(hdc, bkTransparent)

	band := func(top, bottom int32) rect { return rect{rc.left, top, rc.right, bottom} }
	h := rc.bottom - rc.top

	draw := func(font uintptr, color uintptr, text string, r rect, flags uintptr) {
		pSelectObject.Call(hdc, font)
		pSetTextColor.Call(hdc, color)
		rr := r
		pDrawTextW.Call(hdc, uintptr(unsafe.Pointer(u16(text))), ^uintptr(0),
			uintptr(unsafe.Pointer(&rr)), flags)
	}
	const white = 0x00FFFFFF
	const soft = 0x00B0B0B0
	const red = 0x005B5BFF

	center := uintptr(dtCenter | dtVCenter | dtSingleLine | dtNoPrefix)

	draw(fontBig, red, "Time's up", band(int32(float64(h)*0.20), int32(float64(h)*0.38)), center)
	draw(fontMid, white, "Earn more time or ask a parent", band(int32(float64(h)*0.40), int32(float64(h)*0.50)), center)

	pinRow := band(int32(float64(h)*0.56), int32(float64(h)*0.62))
	switch pinState.Load() {
	case pinChecking:
		draw(fontSmall, soft, "checking\u2026", pinRow, center)
	case pinWrong:
		draw(fontSmall, red, "wrong code, try again", pinRow, center)
	default:
		dots := ""
		for range pinBuf {
			dots += "\u25CF "
		}
		if dots == "" {
			draw(fontSmall, soft, "enter parent code, then press Enter", pinRow, center)
		} else {
			draw(fontMid, white, dots, pinRow, center)
		}
	}

	status := "If a parent grants time on their phone, this clears automatically."
	if !gCtrl.Online() {
		status = "Offline: only the local emergency code works right now."
	}
	draw(fontSmall, soft, status, band(int32(float64(h)*0.80), int32(float64(h)*0.86)), center|uintptr(dtWordBreak))

	pEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
}

// hookProc swallows the usual escape hatches while locked. It cannot intercept
// Ctrl+Alt+Del (the Secure Attention Sequence); blocking Task Manager is a
// Group Policy setting, not something a user-mode hook can do.
func hookProc(nCode uintptr, wParam, lParam uintptr) uintptr {
	if int32(nCode) == hcAction && gCtrl != nil && gCtrl.Locked() {
		k := (*kbdLLHookStruct)(unsafe.Pointer(lParam))
		alt := k.flags&llkhfAltDown != 0
		block := false
		switch k.vkCode {
		case vkLWin, vkRWin, vkEscape:
			block = true
		case vkTab:
			block = alt
		case vkF4:
			block = alt
		}
		if block {
			return 1 // eat the keystroke
		}
	}
	r, _, _ := pCallNextHookEx.Call(0, nCode, wParam, lParam)
	return r
}

const taskName = "rewardd-agent"

// installAutostart persists the resolved config to disk and registers a
// scheduled task that launches this agent at any user's logon with highest
// privileges. The task runs the bare exe pointed at the config file, so the API
// token lives only in the config file (not in the task definition) and a parent
// can re-tune settings by editing one file and re-logging-in.
func installAutostart(cfg config, configPath string) error {
	if err := writeConfigFile(configPath, cfg); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	tr := fmt.Sprintf(`"%s" -config "%s"`, exe, configPath)
	cmd := exec.Command("schtasks", "/Create", "/SC", "ONLOGON",
		"/TN", taskName, "/TR", tr, "/RL", "HIGHEST", "/F")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks failed: %v: %s", err, out)
	}
	log.Printf("config written to %s", configPath)
	log.Printf("scheduled task %q created (task action: %s)", taskName, tr)
	log.Printf("inspect with: schtasks /Query /TN %s", taskName)
	return nil
}

// removeAutostart deletes the logon task and the config file. Errors deleting a
// task that does not exist are ignored so uninstall is idempotent.
func removeAutostart(configPath string) error {
	cmd := exec.Command("schtasks", "/Delete", "/TN", taskName, "/F")
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("schtasks delete (ignored): %v: %s", err, out)
	}
	if configPath != "" {
		if err := os.Remove(configPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove config: %w", err)
		}
	}
	return nil
}
