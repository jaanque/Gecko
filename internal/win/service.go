//go:build windows

package win

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	shell32DLL         = syscall.NewLazyDLL("shell32.dll")
	procDragAcceptFiles = shell32DLL.NewProc("DragAcceptFiles")
	procDragQueryFileW  = shell32DLL.NewProc("DragQueryFileW")
	procDragFinish      = shell32DLL.NewProc("DragFinish")
	procShellExecuteExW = shell32DLL.NewProc("ShellExecuteExW")

	procOpenInputDesktop = user32GUI.NewProc("OpenInputDesktop")
	procSetThreadDesktop = user32GUI.NewProc("SetThreadDesktop")
	procCloseDesktop     = user32GUI.NewProc("CloseDesktop")

	advapi32DLL                   = syscall.NewLazyDLL("advapi32.dll")
	procOpenSCManagerW            = advapi32DLL.NewProc("OpenSCManagerW")
	procCreateServiceW            = advapi32DLL.NewProc("CreateServiceW")
	procOpenServiceW              = advapi32DLL.NewProc("OpenServiceW")
	procDeleteService             = advapi32DLL.NewProc("DeleteService")
	procCloseServiceHandle        = advapi32DLL.NewProc("CloseServiceHandle")
	procStartServiceCtrlDispatcher = advapi32DLL.NewProc("StartServiceCtrlDispatcherW")
)

const (
	desktopSwitchDesktop = 0x0100
	desktopGenericAll    = 0x10000000

	scManagerAllAccess = 0xF003F
	serviceAllAccess   = 0xF01FF
	serviceWin32Own    = 0x00000010
	serviceAutoStart   = 0x00000002
	serviceErrorNormal = 0x00000001

	seeMaskNoCloseProcess = 0x00000040
)

type shellExecuteInfo struct {
	CbSize         uint32
	FMask          uint32
	Hwnd           uintptr
	LpVerb         *uint16
	LpFile         *uint16
	LpParameters   *uint16
	LpDirectory    *uint16
	NShow          int32
	HInstApp       uintptr
	LpIDList       uintptr
	LpClass        *uint16
	HkeyClass      uintptr
	DwHotKey       uint32
	HIconOrMonitor uintptr
	HProcess       uintptr
}

// SwitchThreadToInputDesktop attaches the calling thread to the currently active desktop
// (e.g. Winlogon, UAC Secure Desktop, or default user desktop). This allows capture and input
// injection to continue seamlessly when Windows prompts for UAC elevation or is locked.
func SwitchThreadToInputDesktop() {
	hDesk, _, _ := procOpenInputDesktop.Call(0, 0, desktopSwitchDesktop|desktopGenericAll)
	if hDesk != 0 {
		procSetThreadDesktop.Call(hDesk)
		procCloseDesktop.Call(hDesk)
	}
}

// RequestUACElevation launches a new elevated instance of the current executable using
// ShellExecuteEx with the "runas" verb, prompting the local user with the Windows UAC dialog.
func RequestUACElevation() error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}

	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(exePath)
	params, _ := syscall.UTF16PtrFromString("--elevated")

	var sei shellExecuteInfo
	sei.CbSize = uint32(unsafe.Sizeof(sei))
	sei.FMask = seeMaskNoCloseProcess
	sei.LpVerb = verb
	sei.LpFile = file
	sei.LpParameters = params
	sei.NShow = swShowNormal

	r, _, err := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&sei)))
	if r == 0 {
		return fmt.Errorf("UAC elevation denied or failed: %w", err)
	}
	return nil
}

// InstallWindowsService registers the client as a Windows auto-starting service.
func InstallWindowsService(serviceName, displayName string) error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}

	scm, _, _ := procOpenSCManagerW.Call(0, 0, scManagerAllAccess)
	if scm == 0 {
		return errors.New("OpenSCManager failed (requires Administrator privileges)")
	}
	defer procCloseServiceHandle.Call(scm)

	sName, _ := syscall.UTF16PtrFromString(serviceName)
	dName, _ := syscall.UTF16PtrFromString(displayName)
	binPath, _ := syscall.UTF16PtrFromString(fmt.Sprintf("\"%s\" --service", exePath))

	svc, _, err := procCreateServiceW.Call(
		scm,
		uintptr(unsafe.Pointer(sName)),
		uintptr(unsafe.Pointer(dName)),
		serviceAllAccess,
		serviceWin32Own,
		serviceAutoStart,
		serviceErrorNormal,
		uintptr(unsafe.Pointer(binPath)),
		0, 0, 0, 0, 0,
	)
	if svc == 0 {
		return fmt.Errorf("CreateService failed: %w", err)
	}
	procCloseServiceHandle.Call(svc)
	return nil
}

// UninstallWindowsService removes the registered Windows service.
func UninstallWindowsService(serviceName string) error {
	scm, _, _ := procOpenSCManagerW.Call(0, 0, scManagerAllAccess)
	if scm == 0 {
		return errors.New("OpenSCManager failed (requires Administrator privileges)")
	}
	defer procCloseServiceHandle.Call(scm)

	sName, _ := syscall.UTF16PtrFromString(serviceName)
	svc, _, err := procOpenServiceW.Call(scm, uintptr(unsafe.Pointer(sName)), serviceAllAccess)
	if svc == 0 {
		return fmt.Errorf("OpenService failed: %w", err)
	}
	defer procCloseServiceHandle.Call(svc)

	r, _, err := procDeleteService.Call(svc)
	if r == 0 {
		return fmt.Errorf("DeleteService failed: %w", err)
	}
	return nil
}
