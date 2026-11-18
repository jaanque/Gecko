//go:build linux

package linux

import (
	"encoding/binary"
	"errors"
	"time"

	"remote-control/internal/clientui"
	"remote-control/internal/video"
)

// ScreenCapturer captures a specific X11 monitor or the full virtual desktop
// using pure-Go X11 GetImage + XFixes cursor overlay + shared hybrid tile encoding.
type ScreenCapturer struct {
	xc          *x11Conn
	xfixesOK    bool
	xfixesMajor byte

	originX  int
	originY  int
	width    int
	height   int
	currBGRA []byte

	lastMonIdx   int
	lastMonCheck time.Time
	cachedBounds clientui.MonitorBounds

	encoder *video.TileEncoder
}

// NewScreenCapturer constructs a reusable Linux X11 ScreenCapturer.
func NewScreenCapturer() *ScreenCapturer {
	return &ScreenCapturer{
		lastMonIdx: -999,
		encoder:    video.NewTileEncoder(),
	}
}

// Close releases X11 connection and frame buffers.
func (sc *ScreenCapturer) Close() {
	if sc.xc != nil {
		_ = sc.xc.Close()
		sc.xc = nil
	}
	sc.xfixesOK = false
	sc.currBGRA = nil
	if sc.encoder != nil {
		sc.encoder.Close()
	}
}

func (sc *ScreenCapturer) ensureConn() error {
	if sc.xc != nil {
		return nil
	}
	xc, err := dialX11()
	if err != nil {
		return err
	}
	sc.xc = xc
	sc.xfixesOK = false

	if present, major, _, _, err := xc.QueryExtension("XFIXES"); err == nil && present {
		// XFixesQueryVersion (minor 0): ask for 4.0
		var req [12]byte
		req[0] = major
		req[1] = 0
		binary.LittleEndian.PutUint16(req[2:4], 3)
		binary.LittleEndian.PutUint32(req[4:8], 4)
		binary.LittleEndian.PutUint32(req[8:12], 0)
		if _, err := xc.roundTrip(req[:]); err == nil {
			sc.xfixesOK = true
			sc.xfixesMajor = major
		}
	}
	return nil
}

// CaptureFrame captures the selected monitor (0 = Monitor 1, -1 = all monitors) and returns
// an encoded MsgFrame payload containing only changed 64x64 tiles. Returns nil payload if unchanged.
func (sc *ScreenCapturer) CaptureFrame(frameID uint32, monitorIndex, quality int, forceFull bool) ([]byte, int, int, error) {
	if err := sc.ensureConn(); err != nil {
		return nil, 0, 0, err
	}

	now := time.Now()
	if monitorIndex != sc.lastMonIdx || now.Sub(sc.lastMonCheck) > 2*time.Second || sc.cachedBounds.Width <= 0 {
		sc.cachedBounds = resolveMonitorBoundsOnConn(sc.xc, monitorIndex)
		sc.lastMonIdx = monitorIndex
		sc.lastMonCheck = now
	}

	vx := sc.cachedBounds.X
	vy := sc.cachedBounds.Y
	w := sc.cachedBounds.Width
	h := sc.cachedBounds.Height
	if w <= 0 || h <= 0 || w > 16384 || h > 16384 {
		return nil, 0, 0, errors.New("mides de captura invàlides")
	}

	// Clamp to X11 root bounds so GetImage never fails with BadMatch.
	if vx < 0 {
		w += vx
		vx = 0
	}
	if vy < 0 {
		h += vy
		vy = 0
	}
	if vx+w > int(sc.xc.rootW) {
		w = int(sc.xc.rootW) - vx
	}
	if vy+h > int(sc.xc.rootH) {
		h = int(sc.xc.rootH) - vy
	}
	if w <= 0 || h <= 0 {
		return nil, 0, 0, errors.New("regió fora de pantalla")
	}

	if vx != sc.originX || vy != sc.originY || w != sc.width || h != sc.height {
		sc.originX = vx
		sc.originY = vy
		sc.width = w
		sc.height = h
		sc.currBGRA = make([]byte, w*h*4)
		sc.encoder.Reset()
	}

	raw, err := sc.xc.GetImage(sc.xc.root, int16(vx), int16(vy), uint16(w), uint16(h))
	if err != nil {
		_ = sc.xc.Close()
		sc.xc = nil
		return nil, w, h, err
	}

	bppBytes := int(sc.xc.rootBPP) / 8
	if bppBytes != 4 && bppBytes != 3 {
		bppBytes = 4
	}
	if len(raw) < w*h*bppBytes {
		return nil, w, h, errors.New("imatge X11 incompleta")
	}

	if bppBytes == 4 && !sc.xc.imageOrderMSB && (sc.xc.redMask == 0x00FF0000 || sc.xc.redMask == 0) {
		copy(sc.currBGRA, raw[:w*h*4])
		// Normalize alpha channel to 255 so dirty-tile diffs aren't confused by undefined X11 pad bytes.
		for i := 3; i < len(sc.currBGRA); i += 4 {
			sc.currBGRA[i] = 255
		}
	} else {
		convertX11ToBGRA(sc.currBGRA, raw, w*h, bppBytes, sc.xc.imageOrderMSB, sc.xc.redMask, sc.xc.greenMask, sc.xc.blueMask)
	}

	sc.overlayCursor(vx, vy, w, h)

	pkt := sc.encoder.Encode(frameID, sc.currBGRA, w, h, quality, forceFull)
	return pkt, w, h, nil
}

func convertX11ToBGRA(dst, src []byte, count, bppBytes int, msb bool, rMask, gMask, bMask uint32) {
	rShift := maskShift(rMask, 16)
	gShift := maskShift(gMask, 8)
	bShift := maskShift(bMask, 0)
	for i := 0; i < count; i++ {
		var px uint32
		if bppBytes == 4 {
			if msb {
				px = binary.BigEndian.Uint32(src[i*4 : i*4+4])
			} else {
				px = binary.LittleEndian.Uint32(src[i*4 : i*4+4])
			}
		} else {
			if msb {
				px = uint32(src[i*3+0])<<16 | uint32(src[i*3+1])<<8 | uint32(src[i*3+2])
			} else {
				px = uint32(src[i*3+0]) | uint32(src[i*3+1])<<8 | uint32(src[i*3+2])<<16
			}
		}
		dst[i*4+0] = byte((px & bMask) >> bShift)
		dst[i*4+1] = byte((px & gMask) >> gShift)
		dst[i*4+2] = byte((px & rMask) >> rShift)
		dst[i*4+3] = 255
	}
}

func (sc *ScreenCapturer) overlayCursor(vx, vy, w, h int) {
	if sc.xfixesOK {
		// XFixesGetCursorImage (minor 4)
		var req [4]byte
		req[0] = sc.xfixesMajor
		req[1] = 4
		binary.LittleEndian.PutUint16(req[2:4], 1)
		if rep, err := sc.xc.roundTrip(req[:]); err == nil && len(rep) >= 32 {
			cx := int(int16(binary.LittleEndian.Uint16(rep[8:10])))
			cy := int(int16(binary.LittleEndian.Uint16(rep[10:12])))
			cw := int(binary.LittleEndian.Uint16(rep[12:14]))
			ch := int(binary.LittleEndian.Uint16(rep[14:16]))
			xhot := int(binary.LittleEndian.Uint16(rep[16:18]))
			yhot := int(binary.LittleEndian.Uint16(rep[18:20]))
			pix := rep[32:]
			if cw > 0 && ch > 0 && cw <= 256 && ch <= 256 && len(pix) >= cw*ch*4 {
				dx0 := cx - xhot - vx
				dy0 := cy - yhot - vy
				for y := 0; y < ch; y++ {
					dy := dy0 + y
					if dy < 0 || dy >= h {
						continue
					}
					for x := 0; x < cw; x++ {
						dx := dx0 + x
						if dx < 0 || dx >= w {
							continue
						}
						off := (y*cw + x) * 4
						argb := binary.LittleEndian.Uint32(pix[off : off+4])
						a := argb >> 24
						if a == 0 {
							continue
						}
						r := (argb >> 16) & 0xFF
						g := (argb >> 8) & 0xFF
						b := argb & 0xFF
						dstOff := (dy*w + dx) * 4
						if a == 255 {
							sc.currBGRA[dstOff+0] = byte(b)
							sc.currBGRA[dstOff+1] = byte(g)
							sc.currBGRA[dstOff+2] = byte(r)
						} else {
							inv := 255 - a
							sc.currBGRA[dstOff+0] = byte(min(255, b+(uint32(sc.currBGRA[dstOff+0])*inv)/255))
							sc.currBGRA[dstOff+1] = byte(min(255, g+(uint32(sc.currBGRA[dstOff+1])*inv)/255))
							sc.currBGRA[dstOff+2] = byte(min(255, r+(uint32(sc.currBGRA[dstOff+2])*inv)/255))
						}
					}
				}
				return
			}
		}
	}

	// Fallback if XFIXES is unavailable: draw a crisp pointer arrow using QueryPointer.
	px, py, err := sc.xc.QueryPointer(sc.xc.root)
	if err != nil {
		return
	}
	drawFallbackArrow(sc.currBGRA, w, h, int(px)-vx, int(py)-vy)
}

var fallbackArrow = []string{
	"B...........",
	"BB..........",
	"BWB.........",
	"BWWB........",
	"BWWWB.......",
	"BWWWWB......",
	"BWWWWWB.....",
	"BWWWWWWB....",
	"BWWWWWWWB...",
	"BWWWWWWWWB..",
	"BWWWWWBBBBB.",
	"BWWBWWB.....",
	"BWB.BWWB....",
	"BB..BWWB....",
	".....BWWB...",
	"......BB....",
}

func drawFallbackArrow(bgra []byte, w, h, ox, oy int) {
	for y, row := range fallbackArrow {
		dy := oy + y
		if dy < 0 || dy >= h {
			continue
		}
		for x := 0; x < len(row); x++ {
			dx := ox + x
			if dx < 0 || dx >= w {
				continue
			}
			ch := row[x]
			if ch == '.' {
				continue
			}
			off := (dy*w + dx) * 4
			if ch == 'B' {
				bgra[off+0], bgra[off+1], bgra[off+2] = 0, 0, 0
			} else {
				bgra[off+0], bgra[off+1], bgra[off+2] = 255, 255, 255
			}
		}
	}
}
