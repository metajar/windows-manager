//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// These live in DLLs not fully surfaced by x/sys/windows, so we bind them by
// hand (the same lazy-proc style as the lock overlay).
var (
	libKernel32 = windows.NewLazySystemDLL("kernel32.dll")
	libWtsapi32 = windows.NewLazySystemDLL("wtsapi32.dll")
	libUserenv  = windows.NewLazySystemDLL("userenv.dll")

	procWTSGetActiveConsoleSessionId = libKernel32.NewProc("WTSGetActiveConsoleSessionId")
	procWTSQueryUserToken            = libWtsapi32.NewProc("WTSQueryUserToken")
	procCreateEnvironmentBlock       = libUserenv.NewProc("CreateEnvironmentBlock")
	procDestroyEnvironmentBlock      = libUserenv.NewProc("DestroyEnvironmentBlock")
)

const invalidSession = 0xFFFFFFFF

// launchFaceInActiveSession starts "agent -face" inside the interactive desktop
// of the currently logged-in user and blocks until that process exits (so the
// caller can relaunch it). The brain runs as SYSTEM in session 0, which cannot
// draw UI; this CreateProcessAsUser dance is how a session-0 service places a
// process on the user's visible desktop.
func launchFaceInActiveSession(ctx context.Context, configPath string) error {
	r, _, _ := procWTSGetActiveConsoleSessionId.Call()
	session := uint32(r)
	if session == invalidSession {
		return fmt.Errorf("no active console session (nobody logged in)")
	}

	var userTok windows.Token
	if r, _, err := procWTSQueryUserToken.Call(uintptr(session), uintptr(unsafe.Pointer(&userTok))); r == 0 {
		return fmt.Errorf("WTSQueryUserToken: %w", err)
	}
	defer userTok.Close()

	// CreateProcessAsUser needs a primary token.
	var primary windows.Token
	if err := windows.DuplicateTokenEx(
		userTok, windows.TOKEN_ALL_ACCESS, nil,
		windows.SecurityImpersonation, windows.TokenPrimary, &primary,
	); err != nil {
		return fmt.Errorf("DuplicateTokenEx: %w", err)
	}
	defer primary.Close()

	// Build the user's environment so the face inherits their profile. Keep it
	// typed as unsafe.Pointer (not uintptr) so passing it to CreateProcessAsUser
	// below needs no uintptr round-trip.
	var envBlock unsafe.Pointer
	if r, _, err := procCreateEnvironmentBlock.Call(uintptr(unsafe.Pointer(&envBlock)), uintptr(primary), 0); r == 0 {
		return fmt.Errorf("CreateEnvironmentBlock: %w", err)
	}
	defer procDestroyEnvironmentBlock.Call(uintptr(envBlock))

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmdline, err := windows.UTF16PtrFromString(fmt.Sprintf(`"%s" -face -config "%s"`, exe, configPath))
	if err != nil {
		return err
	}
	desktop, err := windows.UTF16PtrFromString(`winsta0\default`)
	if err != nil {
		return err
	}

	var si windows.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Desktop = desktop
	var pi windows.ProcessInformation

	flags := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_NO_WINDOW)
	if err := windows.CreateProcessAsUser(
		primary, nil, cmdline, nil, nil, false,
		flags, (*uint16)(envBlock), nil, &si, &pi,
	); err != nil {
		return fmt.Errorf("CreateProcessAsUser: %w", err)
	}
	defer windows.CloseHandle(pi.Thread)
	defer windows.CloseHandle(pi.Process)

	// Wait for the face to exit, polling so a service stop can terminate it.
	for {
		ev, err := windows.WaitForSingleObject(pi.Process, 1000)
		if err != nil {
			return fmt.Errorf("WaitForSingleObject: %w", err)
		}
		switch ev {
		case windows.WAIT_OBJECT_0:
			return nil // face exited; watchFace relaunches
		case uint32(windows.WAIT_TIMEOUT):
			if ctx.Err() != nil {
				windows.TerminateProcess(pi.Process, 0)
				return nil
			}
		}
	}
}
