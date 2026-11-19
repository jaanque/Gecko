//go:build windows

package relay

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	procOpenSCManagerW            = advapi32.NewProc("OpenSCManagerW")
	procCreateServiceW            = advapi32.NewProc("CreateServiceW")
	procDeleteService             = advapi32.NewProc("DeleteService")
	procOpenServiceW              = advapi32.NewProc("OpenServiceW")
	procControlService            = advapi32.NewProc("ControlService")
	procCloseServiceHandle        = advapi32.NewProc("CloseServiceHandle")
	procStartServiceCtrlDispW     = advapi32.NewProc("StartServiceCtrlDispatcherW")
	procRegisterServiceCtrlHndExW = advapi32.NewProc("RegisterServiceCtrlHandlerExW")
	procSetServiceStatus          = advapi32.NewProc("SetServiceStatus")
)

const (
	scManagerAllAccess = 0xF003F
	serviceAllAccess   = 0xF01FF

	serviceWin32OwnProcess = 0x00000010
	serviceAutoStart       = 0x00000002
	serviceErrorNormal     = 0x00000001

	serviceControlStop     = 0x00000001
	serviceControlShutdown = 0x00000005

	serviceStopped        = 0x00000001
	serviceStartPending   = 0x00000002
	serviceStopPending    = 0x00000003
	serviceRunning        = 0x00000004
	serviceAcceptStop     = 0x00000001
	serviceAcceptShutdown = 0x00000004
)

type serviceStatus struct {
	ServiceType             uint32
	CurrentState            uint32
	ControlsAccepted        uint32
	Win32ExitCode           uint32
	ServiceSpecificExitCode uint32
	CheckPoint              uint32
	WaitHint                uint32
}

type serviceTableEntry struct {
	ServiceName *uint16
	ServiceProc uintptr
}

var (
	activeServiceStatusHandle uintptr
	activeServiceStopCh       chan struct{}
	activeServiceWorker       func(stopCh <-chan struct{})
	activeServiceName         string
)

// InstallService registers RelayServidor as an auto-start Windows Service.
func InstallService(serviceName, displayName string, extraArgs []string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("no se pudo determinar la ruta del ejecutable: %w", err)
	}

	cmdLine := fmt.Sprintf("\"%s\" -service", exe)
	for _, a := range extraArgs {
		cmdLine += fmt.Sprintf(" %s", a)
	}

	scm, _, err := procOpenSCManagerW.Call(0, 0, scManagerAllAccess)
	if scm == 0 {
		return fmt.Errorf("no se pudo abrir el administrador de servicios de Windows (¿ejecutando como Administrador?): %w", err)
	}
	defer procCloseServiceHandle.Call(scm)

	sName, _ := syscall.UTF16PtrFromString(serviceName)
	dName, _ := syscall.UTF16PtrFromString(displayName)
	bPath, _ := syscall.UTF16PtrFromString(cmdLine)

	svc, _, err := procCreateServiceW.Call(
		scm,
		uintptr(unsafe.Pointer(sName)),
		uintptr(unsafe.Pointer(dName)),
		serviceAllAccess,
		serviceWin32OwnProcess,
		serviceAutoStart,
		serviceErrorNormal,
		uintptr(unsafe.Pointer(bPath)),
		0, 0, 0, 0, 0,
	)
	if svc == 0 {
		return fmt.Errorf("error creando servicio %q: %w", serviceName, err)
	}
	procCloseServiceHandle.Call(svc)
	return nil
}

// UninstallService stops and deletes the Windows Service.
func UninstallService(serviceName string) error {
	scm, _, err := procOpenSCManagerW.Call(0, 0, scManagerAllAccess)
	if scm == 0 {
		return fmt.Errorf("no se pudo abrir el administrador de servicios: %w", err)
	}
	defer procCloseServiceHandle.Call(scm)

	sName, _ := syscall.UTF16PtrFromString(serviceName)
	svc, _, err := procOpenServiceW.Call(scm, uintptr(unsafe.Pointer(sName)), serviceAllAccess)
	if svc == 0 {
		return fmt.Errorf("no se encontró el servicio %q: %w", serviceName, err)
	}
	defer procCloseServiceHandle.Call(svc)

	// Stop service if running
	var st serviceStatus
	procControlService.Call(svc, serviceControlStop, uintptr(unsafe.Pointer(&st)))

	r, _, err := procDeleteService.Call(svc)
	if r == 0 {
		return fmt.Errorf("error eliminando servicio: %w", err)
	}
	return nil
}

// RunAsService connects to the Windows Service Control Manager dispatch loop.
func RunAsService(serviceName string, worker func(stopCh <-chan struct{})) error {
	activeServiceName = serviceName
	activeServiceWorker = worker
	activeServiceStopCh = make(chan struct{})

	sName, _ := syscall.UTF16PtrFromString(serviceName)
	table := []serviceTableEntry{
		{
			ServiceName: sName,
			ServiceProc: syscall.NewCallback(serviceMainCallback),
		},
		{ServiceName: nil, ServiceProc: 0},
	}

	r, _, err := procStartServiceCtrlDispW.Call(uintptr(unsafe.Pointer(&table[0])))
	if r == 0 {
		return err
	}
	return nil
}

func serviceMainCallback(argc uint32, argv uintptr) uintptr {
	sName, _ := syscall.UTF16PtrFromString(activeServiceName)
	h, _, _ := procRegisterServiceCtrlHndExW.Call(
		uintptr(unsafe.Pointer(sName)),
		syscall.NewCallback(serviceHandlerCallback),
		0,
	)
	if h == 0 {
		return 0
	}
	activeServiceStatusHandle = h

	setServiceState(serviceStartPending, 0, 3000)
	setServiceState(serviceRunning, serviceAcceptStop|serviceAcceptShutdown, 0)

	if activeServiceWorker != nil {
		activeServiceWorker(activeServiceStopCh)
	}

	setServiceState(serviceStopped, 0, 0)
	return 0
}

func serviceHandlerCallback(dwControl, dwEventType, lpEventData, lpContext uintptr) uintptr {
	switch dwControl {
	case serviceControlStop, serviceControlShutdown:
		setServiceState(serviceStopPending, 0, 5000)
		if activeServiceStopCh != nil {
			select {
			case <-activeServiceStopCh:
			default:
				close(activeServiceStopCh)
			}
		}
		return 0
	}
	return 0
}

func setServiceState(state, accepted, waitHint uint32) {
	if activeServiceStatusHandle == 0 {
		return
	}
	st := serviceStatus{
		ServiceType:      serviceWin32OwnProcess,
		CurrentState:     state,
		ControlsAccepted: accepted,
		WaitHint:         waitHint,
	}
	procSetServiceStatus.Call(activeServiceStatusHandle, uintptr(unsafe.Pointer(&st)))
}
