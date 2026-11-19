//go:build windows

package win

import (
	"hash/fnv"
	"unsafe"

	"remote-control/internal/protocol"
)

// CursorTracker tracks local Windows mouse cursor position and shape changes,
// decoupling cursor motion from video tile re-encoding (RustDesk cursor architecture).
type CursorTracker struct {
	lastHCursor uintptr
	lastX       int
	lastY       int
	lastVisible bool
	lastHash    uint32
	lastWidth   uint16
	lastHeight  uint16
	lastHotX    uint16
	lastHotY    uint16
	hasSentOnce bool
}

// NewCursorTracker constructs a new mouse cursor tracker.
func NewCursorTracker() *CursorTracker {
	return &CursorTracker{}
}

// Poll inspects the current OS cursor state and returns an encoded CursorPacket if
// position or shape changed. Returns nil if unchanged.
func (ct *CursorTracker) Poll(monitorIndex int) (*protocol.CursorPacket, bool) {
	var ci cursorInfo
	ci.CbSize = uint32(unsafe.Sizeof(ci))
	r, _, _ := procGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci)))
	if r == 0 {
		return nil, false
	}

	visible := (ci.Flags & cursorShowing) != 0

	if !visible {
		if !ct.lastVisible && ct.hasSentOnce {
			return nil, false
		}
		ct.lastVisible = false
		ct.hasSentOnce = true
		return &protocol.CursorPacket{
			Visible: false,
		}, true
	}

	bounds := ResolveMonitorBounds(monitorIndex)
	normX, normY := mapScreenToNormalized(int(ci.PtScreenPos.X), int(ci.PtScreenPos.Y), bounds)

	hCursor := ci.HCursor
	posChanged := normX != ct.lastX || normY != ct.lastY
	shapeChanged := hCursor != ct.lastHCursor || !ct.lastVisible || !ct.hasSentOnce

	if !posChanged && !shapeChanged {
		return nil, false
	}

	ct.lastX = normX
	ct.lastY = normY
	ct.lastVisible = true
	ct.hasSentOnce = true

	pkt := &protocol.CursorPacket{
		X:         uint16(normX),
		Y:         uint16(normY),
		Visible:   true,
		HotspotX:  ct.lastHotX,
		HotspotY:  ct.lastHotY,
		Width:     ct.lastWidth,
		Height:    ct.lastHeight,
		ShapeHash: ct.lastHash,
	}

	if shapeChanged && hCursor != 0 {
		w, h, hotX, hotY, bgra, hash := extractCursorShape(hCursor)
		if len(bgra) > 0 {
			ct.lastHCursor = hCursor
			ct.lastHash = hash
			ct.lastWidth = w
			ct.lastHeight = h
			ct.lastHotX = hotX
			ct.lastHotY = hotY

			pkt.Width = w
			pkt.Height = h
			pkt.HotspotX = hotX
			pkt.HotspotY = hotY
			pkt.ShapeHash = hash
			pkt.ShapeBGRA = bgra
		}
	}

	return pkt, true
}

func mapScreenToNormalized(screenX, screenY int, b MonitorBounds) (int, int) {
	if b.Width <= 0 || b.Height <= 0 {
		return 0, 0
	}
	relX := screenX - b.X
	relY := screenY - b.Y
	if relX < 0 {
		relX = 0
	} else if relX >= b.Width {
		relX = b.Width - 1
	}
	if relY < 0 {
		relY = 0
	} else if relY >= b.Height {
		relY = b.Height - 1
	}
	nx := (relX * 65535) / b.Width
	ny := (relY * 65535) / b.Height
	return nx, ny
}

func extractCursorShape(hCursor uintptr) (uint16, uint16, uint16, uint16, []byte, uint32) {
	var ii iconInfo
	if ok, _, _ := procGetIconInfo.Call(hCursor, uintptr(unsafe.Pointer(&ii))); ok == 0 {
		return 0, 0, 0, 0, nil, 0
	}
	defer func() {
		if ii.HbmColor != 0 {
			procDeleteObject.Call(ii.HbmColor)
		}
		if ii.HbmMask != 0 {
			procDeleteObject.Call(ii.HbmMask)
		}
	}()

	const targetDim = 32
	bgra := make([]byte, targetDim*targetDim*4)

	hdcMem, _, _ := procCreateCompatibleDC.Call(0)
	if hdcMem == 0 {
		return 0, 0, 0, 0, nil, 0
	}
	defer procDeleteDC.Call(hdcMem)

	var bi bitmapInfo
	bi.Header.Size = uint32(unsafe.Sizeof(bi.Header))
	bi.Header.Width = targetDim
	bi.Header.Height = -targetDim
	bi.Header.Planes = 1
	bi.Header.BitCount = 32
	bi.Header.Compression = biRGB

	if ii.HbmColor != 0 {
		procGetDIBits.Call(
			hdcMem,
			ii.HbmColor,
			0,
			targetDim,
			uintptr(unsafe.Pointer(&bgra[0])),
			uintptr(unsafe.Pointer(&bi)),
			dibRGBColors,
		)
	} else if ii.HbmMask != 0 {
		// Monochrome cursor: top half is AND mask, bottom half is XOR mask.
		maskBytes := make([]byte, targetDim*targetDim*4)
		procGetDIBits.Call(
			hdcMem,
			ii.HbmMask,
			0,
			targetDim,
			uintptr(unsafe.Pointer(&maskBytes[0])),
			uintptr(unsafe.Pointer(&bi)),
			dibRGBColors,
		)
		for i := 0; i < len(bgra); i += 4 {
			if maskBytes[i] == 0 {
				bgra[i] = 0xFF   // B
				bgra[i+1] = 0xFF // G
				bgra[i+2] = 0xFF // R
				bgra[i+3] = 0xFF // A
			} else {
				bgra[i+3] = 0x00 // Transparent
			}
		}
	}

	h := fnv.New32a()
	h.Write(bgra)
	hash := h.Sum32()

	return targetDim, targetDim, uint16(ii.XHotspot), uint16(ii.YHotspot), bgra, hash
}
