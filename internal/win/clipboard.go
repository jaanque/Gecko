//go:build windows

package win

import (
	"errors"
	"sync"
	"syscall"
	"unsafe"
)

var (
	procEmptyClipboard   = user32GUI.NewProc("EmptyClipboard")
	procSetClipboardData = user32GUI.NewProc("SetClipboardData")
	procGlobalAlloc      = kernel32GUI.NewProc("GlobalAlloc")
	procGlobalFree       = kernel32GUI.NewProc("GlobalFree")
	procGlobalSize       = kernel32GUI.NewProc("GlobalSize")

	clipMu sync.Mutex
)

const (
	gmemMoveable    = 0x0002
	maxClipUTF16Len = 128 * 1024 // 128K UTF-16 code units (~256 KiB)
)

// ReadClipboardText reads UTF-8 text from the Windows clipboard safely from any goroutine.
func ReadClipboardText() string {
	clipMu.Lock()
	defer clipMu.Unlock()
	return readTextFromClipboard(0)
}

// WriteClipboardText places UTF-8 text into the Windows clipboard safely from any goroutine.
func WriteClipboardText(text string) error {
	if text == "" {
		return nil
	}
	u16, err := syscall.UTF16FromString(text)
	if err != nil {
		return err
	}
	if len(u16) > maxClipUTF16Len {
		u16 = append(u16[:maxClipUTF16Len-1], 0)
	}
	byteLen := uintptr(len(u16) * 2)

	clipMu.Lock()
	defer clipMu.Unlock()

	if ok, _, _ := procOpenClipboard.Call(0); ok == 0 {
		return errors.New("OpenClipboard failed")
	}
	defer procCloseClipboard.Call()

	procEmptyClipboard.Call()

	hMem, _, _ := procGlobalAlloc.Call(gmemMoveable, byteLen)
	if hMem == 0 {
		return errors.New("GlobalAlloc failed")
	}
	locked, _, _ := procGlobalLock.Call(hMem)
	if locked == 0 {
		procGlobalFree.Call(hMem)
		return errors.New("GlobalLock failed")
	}
	ptr := *(*unsafe.Pointer)(unsafe.Pointer(&locked))
	dst := unsafe.Slice((*uint16)(ptr), len(u16))
	copy(dst, u16)
	procGlobalUnlock.Call(hMem)

	if res, _, _ := procSetClipboardData.Call(cfUnicodeText, hMem); res == 0 {
		procGlobalFree.Call(hMem)
		return errors.New("SetClipboardData failed")
	}
	return nil
}
