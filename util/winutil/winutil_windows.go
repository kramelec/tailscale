// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package winutil

import (
	"errors"
	"fmt"
	"log"
	"os/exec"
	"runtime"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	regBase       = `SOFTWARE\Tailscale IPN`
	regPolicyBase = `SOFTWARE\Policies\Tailscale`
)

// ErrNoShell is returned when the shell process is not found.
var ErrNoShell = errors.New("no Shell process is present")

// GetDesktopPID searches the PID of the process that's running the
// currently active desktop. Returns ErrNoShell if the shell is not present.
// Usually the PID will be for explorer.exe.
func GetDesktopPID() (uint32, error) {
	hwnd := windows.GetShellWindow()
	if hwnd == 0 {
		return 0, ErrNoShell
	}

	var pid uint32
	windows.GetWindowThreadProcessId(hwnd, &pid)
	if pid == 0 {
		return 0, fmt.Errorf("invalid PID for HWND %v", hwnd)
	}

	return pid, nil
}

func getPolicyString(name, defval string) string {
	s, err := getRegStringInternal(regPolicyBase, name)
	if err != nil {
		// Fall back to the legacy path
		return getRegString(name, defval)
	}
	return s
}

func getPolicyInteger(name string, defval uint64) uint64 {
	i, err := getRegIntegerInternal(regPolicyBase, name)
	if err != nil {
		// Fall back to the legacy path
		return getRegInteger(name, defval)
	}
	return i
}

func getRegString(name, defval string) string {
	s, err := getRegStringInternal(regBase, name)
	if err != nil {
		return defval
	}
	return s
}

func getRegInteger(name string, defval uint64) uint64 {
	i, err := getRegIntegerInternal(regBase, name)
	if err != nil {
		return defval
	}
	return i
}

func getRegStringInternal(subKey, name string) (string, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, subKey, registry.READ)
	if err != nil {
		log.Printf("registry.OpenKey(%v): %v", subKey, err)
		return "", err
	}
	defer key.Close()

	val, _, err := key.GetStringValue(name)
	if err != nil {
		if err != registry.ErrNotExist {
			log.Printf("registry.GetStringValue(%v): %v", name, err)
		}
		return "", err
	}
	return val, nil
}

func getRegIntegerInternal(subKey, name string) (uint64, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, subKey, registry.READ)
	if err != nil {
		log.Printf("registry.OpenKey(%v): %v", subKey, err)
		return 0, err
	}
	defer key.Close()

	val, _, err := key.GetIntegerValue(name)
	if err != nil {
		if err != registry.ErrNotExist {
			log.Printf("registry.GetIntegerValue(%v): %v", name, err)
		}
		return 0, err
	}
	return val, nil
}

var (
	kernel32                         = syscall.NewLazyDLL("kernel32.dll")
	procWTSGetActiveConsoleSessionId = kernel32.NewProc("WTSGetActiveConsoleSessionId")
)

// TODO(crawshaw): replace with x/sys/windows... one day.
// https://go-review.googlesource.com/c/sys/+/331909
func WTSGetActiveConsoleSessionId() uint32 {
	r1, _, _ := procWTSGetActiveConsoleSessionId.Call()
	return uint32(r1)
}

func isSIDValidPrincipal(uid string) bool {
	usid, err := syscall.StringToSid(uid)
	if err != nil {
		return false
	}

	_, _, accType, err := usid.LookupAccount("")
	if err != nil {
		return false
	}

	switch accType {
	case syscall.SidTypeUser, syscall.SidTypeGroup, syscall.SidTypeDomain, syscall.SidTypeAlias, syscall.SidTypeWellKnownGroup, syscall.SidTypeComputer:
		return true
	default:
		// Reject deleted users, invalid SIDs, unknown SIDs, mandatory label SIDs, etc.
		return false
	}
}

// EnableCurrentThreadPrivilege enables the named privilege
// in the current thread access token.
func EnableCurrentThreadPrivilege(name string) error {
	var t windows.Token
	err := windows.OpenThreadToken(windows.CurrentThread(),
		windows.TOKEN_QUERY|windows.TOKEN_ADJUST_PRIVILEGES, false, &t)
	if err != nil {
		return err
	}
	defer t.Close()

	var tp windows.Tokenprivileges

	privStr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	err = windows.LookupPrivilegeValue(nil, privStr, &tp.Privileges[0].Luid)
	if err != nil {
		return err
	}
	tp.PrivilegeCount = 1
	tp.Privileges[0].Attributes = windows.SE_PRIVILEGE_ENABLED
	return windows.AdjustTokenPrivileges(t, false, &tp, 0, nil, nil)
}

// StartProcessAsChild starts exePath process as a child of parentPID.
// StartProcessAsChild copies parentPID's environment variables into
// the new process, along with any optional environment variables in extraEnv.
func StartProcessAsChild(parentPID uint32, exePath string, extraEnv []string) error {
	// The rest of this function requires SeDebugPrivilege to be held.

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	err := windows.ImpersonateSelf(windows.SecurityImpersonation)
	if err != nil {
		return err
	}
	defer windows.RevertToSelf()

	// According to https://docs.microsoft.com/en-us/windows/win32/procthread/process-security-and-access-rights
	//
	// ... To open a handle to another process and obtain full access rights,
	// you must enable the SeDebugPrivilege privilege. ...
	//
	// But we only need PROCESS_CREATE_PROCESS. So perhaps SeDebugPrivilege is too much.
	//
	// https://devblogs.microsoft.com/oldnewthing/20080314-00/?p=23113
	//
	// TODO: try look for something less than SeDebugPrivilege

	err = EnableCurrentThreadPrivilege("SeDebugPrivilege")
	if err != nil {
		return err
	}

	ph, err := windows.OpenProcess(
		windows.PROCESS_CREATE_PROCESS|windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_DUP_HANDLE,
		false, parentPID)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(ph)

	var pt windows.Token
	err = windows.OpenProcessToken(ph, windows.TOKEN_QUERY, &pt)
	if err != nil {
		return err
	}
	defer pt.Close()

	env, err := pt.Environ(false)
	if err != nil {
		return err

	}
	env = append(env, extraEnv...)

	sys := &syscall.SysProcAttr{ParentProcess: syscall.Handle(ph)}

	cmd := exec.Command(exePath)
	cmd.Env = env
	cmd.SysProcAttr = sys

	return cmd.Start()
}

// StartProcessAsCurrentGUIUser is like StartProcessAsChild, but if finds
// current logged in user desktop process (normally explorer.exe),
// and passes found PID to StartProcessAsChild.
func StartProcessAsCurrentGUIUser(exePath string, extraEnv []string) error {
	// as described in https://devblogs.microsoft.com/oldnewthing/20190425-00/?p=102443
	desktop, err := GetDesktopPID()
	if err != nil {
		return fmt.Errorf("failed to find desktop: %v", err)
	}
	err = StartProcessAsChild(desktop, exePath, extraEnv)
	if err != nil {
		return fmt.Errorf("failed to start executable: %v", err)
	}
	return nil
}

// CreateAppMutex creates a named Windows mutex, returning nil if the mutex
// is created successfully or an error if the mutex already exists or could not
// be created for some other reason.
func CreateAppMutex(name string) (windows.Handle, error) {
	return windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(name))
}
