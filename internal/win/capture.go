//go:build windows

package win

import (
	"errors"
	"syscall"
	"time"
	"unsafe"

	"remote-control/internal/video"
)

var (
	user32Cap = syscall.NewLazyDLL("user32.dll")
	gdi32Cap  = syscall.NewLazyDLL("gdi32.dll")

	procGetSystemMetrics = user32Cap.NewProc("GetSystemMetrics")
	procGetDC            = user32Cap.NewProc("GetDC")
	procReleaseDC        = user32Cap.NewProc("ReleaseDC")
	procGetCursorInfo    = user32Cap.NewProc("GetCursorInfo")
	procGetIconInfo      = user32Cap.NewProc("GetIconInfo")
	procDrawIconEx       = user32Cap.NewProc("DrawIconEx")

	procCreateCompatibleDC     = gdi32Cap.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = gdi32Cap.NewProc("CreateCompatibleBitmap")
	procSelectObject           = gdi32Cap.NewProc("SelectObject")
	procBitBlt                 = gdi32Cap.NewProc("BitBlt")
	procGetDIBits              = gdi32Cap.NewProc("GetDIBits")
	procDeleteObject           = gdi32Cap.NewProc("DeleteObject")
	procDeleteDC               = gdi32Cap.NewProc("DeleteDC")
)

const (
	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79

	srccopy       = 0x00CC0020
	captureblt    = 0x40000000
	dibRGBColors  = 0
	biRGB         = 0
	cursorShowing = 0x00000001
	diNormal      = 0x0003
)

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type bitmapInfo struct {
	Header bitmapInfoHeader
	Colors [1]uint32
}

type point struct {
	X int32
	Y int32
}

type cursorInfo struct {
	CbSize      uint32
	Flags       uint32
	HCursor     uintptr
	PtScreenPos point
}

type iconInfo struct {
	FIcon    int32
	XHotspot uint32
	YHotspot uint32
	HbmMask  uintptr
	HbmColor uintptr
}

// ScreenCapturer captures a specific Windows monitor or the full virtual desktop
// using hardware-accelerated DXGI Desktop Duplication (with automatic GDI BitBlt fallback)
// and parallel 64x64 hybrid lossless-UI + JPEG tile encoding.
type ScreenCapturer struct {
	dxgi         dxgiCapturer
	dxgiRetryAt  time.Time
	usingDXGI    bool

	hdcMem   uintptr
	hBitmap  uintptr
	oldObj   uintptr
	originX  int
	originY  int
	width    int
	height   int
	currBGRA []byte
	encoder  *video.TileEncoder
}

// NewScreenCapturer constructs a reusable multi-core ScreenCapturer.
func NewScreenCapturer() *ScreenCapturer {
	return &ScreenCapturer{
		encoder: video.NewTileEncoder(),
	}
}

// Close releases all DXGI and GDI resources.
func (sc *ScreenCapturer) Close() {
	sc.dxgi.Close()
	sc.usingDXGI = false
	sc.releaseGDI()
	sc.currBGRA = nil
	if sc.encoder != nil {
		sc.encoder.Close()
	}
}

func (sc *ScreenCapturer) releaseGDI() {
	if sc.hdcMem != 0 && sc.oldObj != 0 {
		procSelectObject.Call(sc.hdcMem, sc.oldObj)
		sc.oldObj = 0
	}
	if sc.hBitmap != 0 {
		procDeleteObject.Call(sc.hBitmap)
		sc.hBitmap = 0
	}
	if sc.hdcMem != 0 {
		procDeleteDC.Call(sc.hdcMem)
		sc.hdcMem = 0
	}
	sc.width = 0
	sc.height = 0
}

func (sc *ScreenCapturer) ensureSurface(hdcScreen uintptr, w, h int) error {
	if sc.hdcMem != 0 && sc.hBitmap != 0 && sc.width == w && sc.height == h {
		return nil
	}
	sc.releaseGDI()

	hdcMem, _, _ := procCreateCompatibleDC.Call(hdcScreen)
	if hdcMem == 0 {
		return errors.New("CreateCompatibleDC failed")
	}
	hBmp, _, _ := procCreateCompatibleBitmap.Call(hdcScreen, uintptr(w), uintptr(h))
	if hBmp == 0 {
		procDeleteDC.Call(hdcMem)
		return errors.New("CreateCompatibleBitmap failed")
	}
	oldObj, _, _ := procSelectObject.Call(hdcMem, hBmp)

	sc.hdcMem = hdcMem
	sc.hBitmap = hBmp
	sc.oldObj = oldObj
	sc.width = w
	sc.height = h
	sc.currBGRA = make([]byte, w*h*4)
	sc.encoder.Reset()
	return nil
}

// CaptureFrame captures the selected monitor (0 = Monitor 1, -1 = all monitors) and returns
// an encoded MsgFrame payload containing only changed 64x64 tiles. Returns nil payload if unchanged.
// Single-monitor captures use GPU DXGI Desktop Duplication with dirty-rect fast-path and fall back
// transparently to GDI BitBlt on virtual multi-monitor views or unsupported sessions (e.g. RDP/VM).
func (sc *ScreenCapturer) CaptureFrame(frameID uint32, monitorIndex, quality int, forceFull bool) ([]byte, int, int, error) {
	bounds := ResolveMonitorBounds(monitorIndex)
	vx := bounds.X
	vy := bounds.Y
	w := bounds.Width
	h := bounds.Height
	if w <= 0 || h <= 0 || w > 16384 || h > 16384 {
		return nil, 0, 0, errors.New("invalid capture dimensions")
	}
	if vx != sc.originX || vy != sc.originY {
		sc.originX = vx
		sc.originY = vy
		sc.encoder.Reset()
	}

	// Prefer hardware DXGI Desktop Duplication for single-monitor capture.
	if monitorIndex >= 0 && time.Now().After(sc.dxgiRetryAt) {
		bgra, dirtyRects, changed, err := sc.dxgi.AcquireFrame(vx, vy, w, h, forceFull)
		if err == nil {
			if !sc.usingDXGI {
				sc.usingDXGI = true
				sc.releaseGDI()
			}
			if !changed {
				return nil, w, h, nil
			}
			pkt := sc.encoder.EncodeDirtyRects(frameID, bgra, w, h, quality, dirtyRects, forceFull)
			return pkt, w, h, nil
		}
		// Back off DXGI retries briefly and fall back transparently to GDI BitBlt.
		sc.usingDXGI = false
		sc.dxgi.Close()
		sc.dxgiRetryAt = time.Now().Add(3 * time.Second)
	} else if monitorIndex < 0 && sc.usingDXGI {
		sc.usingDXGI = false
		sc.dxgi.Close()
	}

	hdcScreen, _, _ := procGetDC.Call(0)
	if hdcScreen == 0 {
		return nil, w, h, errors.New("GetDC failed")
	}
	defer procReleaseDC.Call(0, hdcScreen)

	if err := sc.ensureSurface(hdcScreen, w, h); err != nil {
		return nil, w, h, err
	}

	r, _, _ := procBitBlt.Call(
		sc.hdcMem, 0, 0, uintptr(w), uintptr(h),
		hdcScreen, uintptr(int32(vx)), uintptr(int32(vy)),
		srccopy|captureblt,
	)
	if r == 0 {
		return nil, w, h, errors.New("BitBlt failed (desktop may be locked or UAC active)")
	}

	drawCursor(sc.hdcMem, vx, vy)

	var bi bitmapInfo
	bi.Header.Size = uint32(unsafe.Sizeof(bi.Header))
	bi.Header.Width = int32(w)
	bi.Header.Height = -int32(h) // top-down DIB
	bi.Header.Planes = 1
	bi.Header.BitCount = 32
	bi.Header.Compression = biRGB

	scanLines, _, _ := procGetDIBits.Call(
		sc.hdcMem,
		sc.hBitmap,
		0,
		uintptr(h),
		uintptr(unsafe.Pointer(&sc.currBGRA[0])),
		uintptr(unsafe.Pointer(&bi)),
		dibRGBColors,
	)
	if scanLines == 0 {
		return nil, w, h, errors.New("GetDIBits failed")
	}

	pkt := sc.encoder.Encode(frameID, sc.currBGRA, w, h, quality, forceFull)
	return pkt, w, h, nil
}

func drawCursor(hdc uintptr, vx, vy int) {
	var ci cursorInfo
	ci.CbSize = uint32(unsafe.Sizeof(ci))
	r, _, _ := procGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci)))
	if r == 0 || (ci.Flags&cursorShowing) == 0 || ci.HCursor == 0 {
		return
	}

	x := int(ci.PtScreenPos.X) - vx
	y := int(ci.PtScreenPos.Y) - vy

	var ii iconInfo
	if ok, _, _ := procGetIconInfo.Call(ci.HCursor, uintptr(unsafe.Pointer(&ii))); ok != 0 {
		x -= int(ii.XHotspot)
		y -= int(ii.YHotspot)
		if ii.HbmMask != 0 {
			procDeleteObject.Call(ii.HbmMask)
		}
		if ii.HbmColor != 0 {
			procDeleteObject.Call(ii.HbmColor)
		}
	}

	procDrawIconEx.Call(hdc, uintptr(int32(x)), uintptr(int32(y)), ci.HCursor, 0, 0, 0, 0, diNormal)
}
