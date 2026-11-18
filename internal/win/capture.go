//go:build windows

package win

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"image"
	"image/jpeg"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"remote-control/internal/protocol"
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

	tileSize = 64
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

type tileJob struct {
	tx, ty, tw, th int
}

type encodedTile struct {
	tx, ty, tw, th uint16
	codec          uint8
	data           []byte
}

type tileEncoderWorker struct {
	tileRGBA *image.RGBA
	bgrBuf   []byte
	compBuf  bytes.Buffer
	zw       *zlib.Writer
}

func newTileEncoderWorker() *tileEncoderWorker {
	w := &tileEncoderWorker{
		tileRGBA: image.NewRGBA(image.Rect(0, 0, tileSize, tileSize)),
		bgrBuf:   make([]byte, tileSize*tileSize*3),
	}
	w.zw, _ = zlib.NewWriterLevel(&w.compBuf, zlib.BestSpeed)
	return w
}

func (w *tileEncoderWorker) encode(currBGRA []byte, stride int, job tileJob, quality int) (encodedTile, bool) {
	tx, ty, tw, th := job.tx, job.ty, job.tw, job.th

	// In Estándar (78) and Alta (92) modes, encode crisp UI/text tiles losslessly via fast Zlib-BGR
	// so small text and ClearType edges stay 100% razor-sharp without JPEG chroma blur.
	if quality >= 70 && isCrispUITile(currBGRA, stride, tx, ty, tw, th) {
		bgr := w.bgrBuf[:tw*th*3]
		p := 0
		for row := 0; row < th; row++ {
			srcOff := (ty+row)*stride + tx*4
			srcRow := currBGRA[srcOff : srcOff+tw*4]
			for i := 0; i < tw*4; i += 4 {
				bgr[p+0] = srcRow[i+0] // B
				bgr[p+1] = srcRow[i+1] // G
				bgr[p+2] = srcRow[i+2] // R
				p += 3
			}
		}

		w.compBuf.Reset()
		w.zw.Reset(&w.compBuf)
		_, err1 := w.zw.Write(bgr)
		err2 := w.zw.Close()
		if err1 == nil && err2 == nil {
			comp := w.compBuf.Bytes()
			// Only keep lossless if it actually compresses cleanly (flat UI/text tiles compress to < 1.8 B/px).
			if len(comp) > 0 && len(comp) <= (tw*th*9)/5 {
				out := make([]byte, len(comp))
				copy(out, comp)
				return encodedTile{
					tx: uint16(tx), ty: uint16(ty), tw: uint16(tw), th: uint16(th),
					codec: protocol.TileCodecZlibBGR,
					data:  out,
				}, true
			}
		}
	}

	// Photographic / video / motion tiles -> fast JPEG.
	subImg := w.tileRGBA.SubImage(image.Rect(0, 0, tw, th)).(*image.RGBA)
	for row := 0; row < th; row++ {
		srcOff := (ty+row)*stride + tx*4
		dstOff := row * subImg.Stride
		srcRow := currBGRA[srcOff : srcOff+tw*4]
		dstRow := subImg.Pix[dstOff : dstOff+tw*4]
		for i := 0; i < tw*4; i += 4 {
			dstRow[i+0] = srcRow[i+2] // R
			dstRow[i+1] = srcRow[i+1] // G
			dstRow[i+2] = srcRow[i+0] // B
			dstRow[i+3] = 255
		}
	}

	w.compBuf.Reset()
	if err := jpeg.Encode(&w.compBuf, subImg, &jpeg.Options{Quality: quality}); err != nil {
		return encodedTile{}, false
	}
	jpg := w.compBuf.Bytes()
	out := make([]byte, len(jpg))
	copy(out, jpg)
	return encodedTile{
		tx: uint16(tx), ty: uint16(ty), tw: uint16(tw), th: uint16(th),
		codec: protocol.TileCodecJPEG,
		data:  out,
	}, true
}

// isCrispUITile checks in microseconds whether a tile looks like crisp UI/text
// (high horizontal pixel-run coherence typical of editors, documents, menus, and ClearType text).
func isCrispUITile(bgra []byte, stride, tx, ty, tw, th int) bool {
	if tw < 4 || th < 4 {
		return false
	}
	same := 0
	total := 0
	for row := 0; row < th; row += 2 {
		off := (ty+row)*stride + tx*4
		prevB, prevG, prevR := bgra[off], bgra[off+1], bgra[off+2]
		for x := 1; x < tw; x++ {
			o := off + x*4
			b, g, r := bgra[o], bgra[o+1], bgra[o+2]
			if b == prevB && g == prevG && r == prevR {
				same++
			}
			prevB, prevG, prevR = b, g, r
			total++
		}
	}
	return total > 0 && same*100 >= total*48
}

// ScreenCapturer captures a specific Windows monitor or the full virtual desktop
// using parallel 64x64 hybrid lossless-UI + JPEG tile encoding.
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

	workers []*tileEncoderWorker
	jobs    []tileJob
	results []encodedTile
	outBuf  bytes.Buffer
}

// NewScreenCapturer constructs a reusable multi-core ScreenCapturer.
func NewScreenCapturer() *ScreenCapturer {
	n := runtime.NumCPU()
	if n > 8 {
		n = 8
	}
	if n < 1 {
		n = 1
	}
	workers := make([]*tileEncoderWorker, n)
	for i := range workers {
		workers[i] = newTileEncoderWorker()
	}
	return &ScreenCapturer{
		workers: workers,
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
// an encoded MsgFrame payload containing only changed 64x64 tiles. Returns nil payload if unchanged.
func (sc *ScreenCapturer) CaptureFrame(frameID uint32, sentUnixNano int64, monitorIndex, quality int, forceFull bool) ([]byte, int, int, error) {
	if quality < 35 {
		quality = 35
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

	sc.jobs = sc.jobs[:0]
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
			if sendAll || tileChanged(sc.currBGRA, sc.prevBGRA, stride, tx, ty, tw, th) {
				sc.jobs = append(sc.jobs, tileJob{tx: tx, ty: ty, tw: tw, th: th})
			}
		}
	}

	// Swap buffers so prevBGRA always holds the latest captured screen.
	curr := sc.currBGRA
	sc.currBGRA, sc.prevBGRA = sc.prevBGRA, sc.currBGRA
	sc.hasPrev = true

	jobCount := len(sc.jobs)
	if jobCount == 0 {
		return nil, w, h, nil
	}

	if cap(sc.results) < jobCount {
		sc.results = make([]encodedTile, jobCount)
	} else {
		sc.results = sc.results[:jobCount]
	}

	// Encode dirty tiles in parallel when multiple tiles changed.
	if jobCount <= 3 || len(sc.workers) == 1 {
		worker := sc.workers[0]
		for i, job := range sc.jobs {
			if enc, ok := worker.encode(curr, stride, job, quality); ok {
				sc.results[i] = enc
			} else {
				sc.results[i] = encodedTile{}
			}
		}
	} else {
		workerCount := len(sc.workers)
		if workerCount > jobCount {
			workerCount = jobCount
		}
		var nextIdx atomic.Int64
		var wg sync.WaitGroup
		wg.Add(workerCount)
		for wi := 0; wi < workerCount; wi++ {
			worker := sc.workers[wi]
			go func() {
				defer wg.Done()
				for {
					idx := int(nextIdx.Add(1) - 1)
					if idx >= jobCount {
						return
					}
					if enc, ok := worker.encode(curr, stride, sc.jobs[idx], quality); ok {
						sc.results[idx] = enc
					} else {
						sc.results[idx] = encodedTile{}
					}
				}
			}()
		}
		wg.Wait()
	}

	// Serialize frame packet.
	sc.outBuf.Reset()
	var frameHdr [protocol.FrameHeaderSize]byte
	sc.outBuf.Write(frameHdr[:])

	var validTiles uint16
	var thdr [protocol.TileHeaderSize]byte
	for i := range sc.results {
		t := &sc.results[i]
		if len(t.data) == 0 {
			continue
		}
		binary.BigEndian.PutUint16(thdr[0:2], t.tx)
		binary.BigEndian.PutUint16(thdr[2:4], t.ty)
		binary.BigEndian.PutUint16(thdr[4:6], t.tw)
		binary.BigEndian.PutUint16(thdr[6:8], t.th)
		thdr[8] = t.codec
		binary.BigEndian.PutUint32(thdr[9:13], uint32(len(t.data)))
		sc.outBuf.Write(thdr[:])
		sc.outBuf.Write(t.data)
		t.data = nil // release reference promptly
		validTiles++
	}

	if validTiles == 0 {
		return nil, w, h, nil
	}

	raw := sc.outBuf.Bytes()
	binary.BigEndian.PutUint32(raw[0:4], frameID)
	binary.BigEndian.PutUint64(raw[4:12], uint64(sentUnixNano))
	binary.BigEndian.PutUint16(raw[12:14], uint16(w))
	binary.BigEndian.PutUint16(raw[14:16], uint16(h))
	binary.BigEndian.PutUint16(raw[16:18], validTiles)

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
