//go:build windows

package win

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/jpeg"
	"syscall"
	"unsafe"
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

	tileSize = 128
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

// ScreenSize returns the current virtual desktop width and height in physical pixels.
func ScreenSize() (int, int) {
	w, _, _ := procGetSystemMetrics.Call(smCXVirtualScreen)
	h, _, _ := procGetSystemMetrics.Call(smCYVirtualScreen)
	return int(int32(w)), int(int32(h))
}

// ScreenCapturer captures a specific Windows monitor or the full virtual desktop with dirty-tile JPEG diffing.
type ScreenCapturer struct {
	hdcMem   uintptr
	hBitmap  uintptr
	oldObj   uintptr
	originX  int
	originY  int
	width    int
	height   int
	currBGRA []byte
	prevBGRA []byte
	hasPrev  bool

	tileRGBA *image.RGBA
	jpegBuf  bytes.Buffer
	outBuf   bytes.Buffer
}

// NewScreenCapturer constructs a reusable ScreenCapturer.
func NewScreenCapturer() *ScreenCapturer {
	return &ScreenCapturer{
		tileRGBA: image.NewRGBA(image.Rect(0, 0, tileSize, tileSize)),
	}
}

// Close releases all GDI resources.
func (sc *ScreenCapturer) Close() {
	sc.releaseGDI()
	sc.currBGRA = nil
	sc.prevBGRA = nil
	sc.hasPrev = false
}

// Reset forces the next CaptureFrame call to emit a complete keyframe.
func (sc *ScreenCapturer) Reset() {
	sc.hasPrev = false
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

	byteLen := w * h * 4
	sc.currBGRA = make([]byte, byteLen)
	sc.prevBGRA = make([]byte, byteLen)
	sc.hasPrev = false
	return nil
}

// CaptureFrame captures the selected monitor (0 = Monitor 1, -1 = all monitors) and returns
// an encoded MsgFrame payload containing only changed tiles. Returns nil payload if unchanged.
func (sc *ScreenCapturer) CaptureFrame(monitorIndex int, quality int, forceFull bool) ([]byte, int, int, error) {
	if quality < 30 {
		quality = 30
	}
	if quality > 95 {
		quality = 95
	}

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
		sc.hasPrev = false
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

	sendAll := forceFull || !sc.hasPrev
	stride := w * 4

	sc.outBuf.Reset()
	// Reserve 6-byte frame header: width(2), height(2), tileCount(2).
	var frameHdr [6]byte
	sc.outBuf.Write(frameHdr[:])

	var tileCount uint16
	opts := &jpeg.Options{Quality: quality}

	for ty := 0; ty < h; ty += tileSize {
		th := tileSize
		if ty+th > h {
			th = h - ty
		}
		for tx := 0; tx < w; tx += tileSize {
			tw := tileSize
			if tx+tw > w {
				tw = w - tx
			}
			if !sendAll && !tileChanged(sc.currBGRA, sc.prevBGRA, stride, tx, ty, tw, th) {
				continue
			}

			// Copy tile BGRA -> RGBA into reusable tile image.
			subImg := sc.tileRGBA.SubImage(image.Rect(0, 0, tw, th)).(*image.RGBA)
			for row := 0; row < th; row++ {
				srcOff := (ty+row)*stride + tx*4
				dstOff := row * subImg.Stride
				srcRow := sc.currBGRA[srcOff : srcOff+tw*4]
				dstRow := subImg.Pix[dstOff : dstOff+tw*4]
				for i := 0; i < tw*4; i += 4 {
					dstRow[i+0] = srcRow[i+2] // R
					dstRow[i+1] = srcRow[i+1] // G
					dstRow[i+2] = srcRow[i+0] // B
					dstRow[i+3] = 255         // A
				}
			}

			sc.jpegBuf.Reset()
			if err := jpeg.Encode(&sc.jpegBuf, subImg, opts); err != nil {
				continue
			}
			jpg := sc.jpegBuf.Bytes()

			var thdr [12]byte
			binary.BigEndian.PutUint16(thdr[0:2], uint16(tx))
			binary.BigEndian.PutUint16(thdr[2:4], uint16(ty))
			binary.BigEndian.PutUint16(thdr[4:6], uint16(tw))
			binary.BigEndian.PutUint16(thdr[6:8], uint16(th))
			binary.BigEndian.PutUint32(thdr[8:12], uint32(len(jpg)))
			sc.outBuf.Write(thdr[:])
			sc.outBuf.Write(jpg)
			tileCount++
		}
	}

	// Swap current & previous buffers.
	sc.currBGRA, sc.prevBGRA = sc.prevBGRA, sc.currBGRA
	sc.hasPrev = true

	if tileCount == 0 {
		return nil, w, h, nil
	}

	raw := sc.outBuf.Bytes()
	binary.BigEndian.PutUint16(raw[0:2], uint16(w))
	binary.BigEndian.PutUint16(raw[2:4], uint16(h))
	binary.BigEndian.PutUint16(raw[4:6], tileCount)

	// Return an owned copy so the caller can send asynchronously without races.
	result := make([]byte, len(raw))
	copy(result, raw)
	return result, w, h, nil
}

func tileChanged(curr, prev []byte, stride, tx, ty, tw, th int) bool {
	rowBytes := tw * 4
	for row := 0; row < th; row++ {
		off := (ty+row)*stride + tx*4
		if !bytes.Equal(curr[off:off+rowBytes], prev[off:off+rowBytes]) {
			return true
		}
	}
	return false
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
