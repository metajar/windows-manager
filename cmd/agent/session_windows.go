//go:build windows

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// These live in DLLs not fully surfaced by x/sys/windows, so we bind them by
// hand (the same lazy-proc style as the lock overlay).
var (
	libUserenv = windows.NewLazySystemDLL("userenv.dll")

	procCreateEnvironmentBlock  = libUserenv.NewProc("CreateEnvironmentBlock")
	procDestroyEnvironmentBlock = libUserenv.NewProc("DestroyEnvironmentBlock")
)

// activeSessions returns the IDs of every session a user is actively using:
// the physical console and/or RDP sessions. WTSGetActiveConsoleSessionId alone
// was wrong here — while someone is connected over RDP the physical console is
// parked at the logon screen with no user token, so the brain spun forever on
// "token does not exist" and never raised a lock anywhere.
func activeSessions() ([]uint32, error) {
	var info *windows.WTS_SESSION_INFO
	var count uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &info, &count); err != nil {
		return nil, fmt.Errorf("WTSEnumerateSessions: %w", err)
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(info)))

	var out []uint32
	for _, s := range unsafe.Slice(info, count) {
		if s.State == windows.WTSActive {
			out = append(out, s.SessionID)
		}
	}
	return out, nil
}

// launchFaceInSession starts "agent -face" inside the interactive desktop of
// the given session and blocks until that process exits (so the caller can
// relaunch it). The brain runs as SYSTEM in session 0, which cannot draw UI;
// this CreateProcessAsUser dance is how a session-0 service places a process
// on a user's visible desktop.
func launchFaceInSession(ctx context.Context, configPath string, session uint32) error {
	var userTok windows.Token
	if err := windows.WTSQueryUserToken(session, &userTok); err != nil {
		return fmt.Errorf("WTSQueryUserToken(session %d): %w", session, err)
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
	log.Printf("face started in session %d (pid %d)", session, pi.ProcessId)

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
