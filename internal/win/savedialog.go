//go:build windows

package win

import (
	"syscall"
	"unsafe"
)

var (
	comdlg32 = syscall.NewLazyDLL("comdlg32.dll")

	procGetSaveFileNameW = comdlg32.NewProc("GetSaveFileNameW")
	procMessageBoxW      = user32GUI.NewProc("MessageBoxW")
)

const (
	ofnOverwritePrompt = 0x00000002
	ofnPathMustExist   = 0x00000800
	ofnNoChangeDir     = 0x00000008
	ofnExplorer        = 0x00080000

	mbOK        = 0x00000000
	mbIconError = 0x00000010
	mbIconInfo  = 0x00000040
)

type openFileNameW struct {
	LStructSize       uint32
	HwndOwner         uintptr
	HInstance         uintptr
	LpstrFilter       *uint16
	LpstrCustomFilter *uint16
	NMaxCustFilter    uint32
	NFilterIndex      uint32
	LpstrFile         *uint16
	NMaxFile          uint32
	LpstrFileTitle    *uint16
	NMaxFileTitle     uint32
	LpstrInitialDir   *uint16
	LpstrTitle        *uint16
	Flags             uint32
	NFileOffset       uint16
	NFileExtension    uint16
	LpstrDefExt       *uint16
	LCustData         uintptr
	LpfnHook          uintptr
	LpTemplateName    *uint16
	PvReserved        uintptr
	DwReserved        uint32
	FlagsEx           uint32
}

// ShowSaveExeDialog opens the native Windows "Guardar como..." dialog for ClienteRemoto.exe.
// Returns ("", false) if the user cancels.
func ShowSaveExeDialog(ownerHwnd uintptr, defaultFileName string) (string, bool) {
	fileBuf := make([]uint16, 4096)
	if def, err := syscall.UTF16FromString(defaultFileName); err == nil && len(def) < len(fileBuf) {
		copy(fileBuf, def)
	}

	// Double-null-terminated Win32 filter string.
	filterStr := "Ejecutable de Windows (*.exe)\x00*.exe\x00Todos los archivos (*.*)\x00*.*\x00\x00"
	filterUTF16 := utf16WithNulls(filterStr)
	titleUTF16, _ := syscall.UTF16PtrFromString("Guardar cliente remoto")
	defExtUTF16, _ := syscall.UTF16PtrFromString("exe")

	ofn := openFileNameW{
		LStructSize:  uint32(unsafe.Sizeof(openFileNameW{})),
		HwndOwner:    ownerHwnd,
		LpstrFilter:  &filterUTF16[0],
		NFilterIndex: 1,
		LpstrFile:    &fileBuf[0],
		NMaxFile:     uint32(len(fileBuf)),
		LpstrTitle:   titleUTF16,
		Flags:        ofnOverwritePrompt | ofnPathMustExist | ofnNoChangeDir | ofnExplorer,
		LpstrDefExt:  defExtUTF16,
	}

	ok, _, _ := procGetSaveFileNameW.Call(uintptr(unsafe.Pointer(&ofn)))
	if ok == 0 {
		return "", false
	}
	return syscall.UTF16ToString(fileBuf), true
}

// ShowMessageBox displays a native Windows message box.
func ShowMessageBox(ownerHwnd uintptr, title, message string, isError bool) {
	t, _ := syscall.UTF16PtrFromString(title)
	m, _ := syscall.UTF16PtrFromString(message)
	flags := uintptr(mbOK | mbIconInfo)
	if isError {
		flags = uintptr(mbOK | mbIconError)
	}
	procMessageBoxW.Call(ownerHwnd, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), flags)
}

func utf16WithNulls(s string) []uint16 {
	out := make([]uint16, 0, len(s))
	for _, r := range s {
		out = append(out, uint16(r))
	}
	return out
}
