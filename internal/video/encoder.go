package video

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"image"
	"image/jpeg"
	"runtime"
	"sync"

	"remote-control/internal/protocol"
)

const tileSize = 64

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
	// so small text and edges stay razor-sharp without JPEG chroma blur.
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

// TileEncoder maintains previous frame state and encodes changed 64x64 tiles
// into MsgFrame payloads using parallel hybrid lossless-UI + JPEG encoding.
type TileEncoder struct {
	width    int
	height   int
	prevBGRA []byte
	hasPrev  bool

	workers []*tileEncoderWorker
	jobs    []tileJob
	results []encodedTile
	outBuf  bytes.Buffer
}

// NewTileEncoder constructs a reusable multi-core TileEncoder.
func NewTileEncoder() *TileEncoder {
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
	return &TileEncoder{workers: workers}
}

// Reset discards the previous reference frame so the next Encode emits a full keyframe.
func (e *TileEncoder) Reset() {
	e.hasPrev = false
}

// Close releases frame buffers.
func (e *TileEncoder) Close() {
	e.prevBGRA = nil
	e.hasPrev = false
	e.width = 0
	e.height = 0
}

// Encode compares currBGRA (top-down 32-bit BGRA, stride = w*4) against the previous frame
// and returns a serialized MsgFrame payload containing only changed tiles (or nil if unchanged).
func (e *TileEncoder) Encode(frameID uint32, currBGRA []byte, w, h, quality int, forceFull bool) []byte {
	return e.EncodeDirtyRects(frameID, currBGRA, w, h, quality, nil, forceFull)
}

// EncodeDirtyRects encodes changed 64x64 tiles into a serialized MsgFrame payload.
// When dirtyRects is non-empty (e.g. from Windows DXGI Desktop Duplication GPU metadata)
// and !forceFull, only tiles intersecting dirtyRects are inspected and updated, avoiding a
// full-framebuffer scan on high-resolution (4K/HiDPI) monitors.
func (e *TileEncoder) EncodeDirtyRects(frameID uint32, currBGRA []byte, w, h, quality int, dirtyRects []image.Rectangle, forceFull bool) []byte {
	if w <= 0 || h <= 0 || len(currBGRA) < w*h*4 {
		return nil
	}
	if quality < 35 {
		quality = 35
	}
	if quality > 95 {
		quality = 95
	}

	byteLen := w * h * 4
	if e.width != w || e.height != h || len(e.prevBGRA) != byteLen {
		e.prevBGRA = make([]byte, byteLen)
		e.width = w
		e.height = h
		e.hasPrev = false
	}

	sendAll := forceFull || !e.hasPrev
	stride := w * 4

	e.jobs = e.jobs[:0]
	if !sendAll && len(dirtyRects) > 0 {
		cols := (w + tileSize - 1) / tileSize
		rows := (h + tileSize - 1) / tileSize
		visited := make([]bool, cols*rows)
		screenBounds := image.Rect(0, 0, w, h)
		for _, r := range dirtyRects {
			clamped := r.Intersect(screenBounds)
			if clamped.Empty() {
				continue
			}
			c0 := clamped.Min.X / tileSize
			c1 := (clamped.Max.X - 1) / tileSize
			r0 := clamped.Min.Y / tileSize
			r1 := (clamped.Max.Y - 1) / tileSize
			for gy := r0; gy <= r1; gy++ {
				ty := gy * tileSize
				th := tileSize
				if ty+th > h {
					th = h - ty
				}
				for gx := c0; gx <= c1; gx++ {
					idx := gy*cols + gx
					if visited[idx] {
						continue
					}
					visited[idx] = true
					tx := gx * tileSize
					tw := tileSize
					if tx+tw > w {
						tw = w - tx
					}
					if tileChanged(currBGRA, e.prevBGRA, stride, tx, ty, tw, th) {
						copyTile(e.prevBGRA, currBGRA, stride, tx, ty, tw, th)
						e.jobs = append(e.jobs, tileJob{tx: tx, ty: ty, tw: tw, th: th})
					}
				}
			}
		}
	} else {
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
				if sendAll || tileChanged(currBGRA, e.prevBGRA, stride, tx, ty, tw, th) {
					e.jobs = append(e.jobs, tileJob{tx: tx, ty: ty, tw: tw, th: th})
				}
			}
		}
		copy(e.prevBGRA, currBGRA[:byteLen])
	}
	e.hasPrev = true

	jobCount := len(e.jobs)
	if jobCount == 0 {
		return nil
	}

	if cap(e.results) < jobCount {
		e.results = make([]encodedTile, jobCount)
	} else {
		e.results = e.results[:jobCount]
	}

	if jobCount <= 3 || len(e.workers) == 1 {
		worker := e.workers[0]
		for i, job := range e.jobs {
			if enc, ok := worker.encode(e.prevBGRA, stride, job, quality); ok {
				e.results[i] = enc
			} else {
				e.results[i] = encodedTile{}
			}
		}
	} else {
		workerCount := len(e.workers)
		if workerCount > jobCount {
			workerCount = jobCount
		}
		var wg sync.WaitGroup
		wg.Add(workerCount)
		for wi := 0; wi < workerCount; wi++ {
			worker := e.workers[wi]
			go func(start int) {
				defer wg.Done()
				for idx := start; idx < jobCount; idx += workerCount {
					if enc, ok := worker.encode(e.prevBGRA, stride, e.jobs[idx], quality); ok {
						e.results[idx] = enc
					} else {
						e.results[idx] = encodedTile{}
					}
				}
			}(wi)
		}
		wg.Wait()
	}

	e.outBuf.Reset()
	var frameHdr [protocol.FrameHeaderSize]byte
	e.outBuf.Write(frameHdr[:])

	var validTiles uint16
	var thdr [protocol.TileHeaderSize]byte
	for i := range e.results {
		t := &e.results[i]
		if len(t.data) == 0 {
			continue
		}
		binary.BigEndian.PutUint16(thdr[0:2], t.tx)
		binary.BigEndian.PutUint16(thdr[2:4], t.ty)
		binary.BigEndian.PutUint16(thdr[4:6], t.tw)
		binary.BigEndian.PutUint16(thdr[6:8], t.th)
		thdr[8] = t.codec
		binary.BigEndian.PutUint32(thdr[9:13], uint32(len(t.data)))
		e.outBuf.Write(thdr[:])
		e.outBuf.Write(t.data)
		t.data = nil
		validTiles++
	}

	if validTiles == 0 {
		return nil
	}

	raw := e.outBuf.Bytes()
	binary.BigEndian.PutUint32(raw[0:4], frameID)
	binary.BigEndian.PutUint16(raw[4:6], uint16(w))
	binary.BigEndian.PutUint16(raw[6:8], uint16(h))
	binary.BigEndian.PutUint16(raw[8:10], validTiles)

	result := make([]byte, len(raw))
	copy(result, raw)
	return result
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

func copyTile(dst, src []byte, stride, tx, ty, tw, th int) {
	rowBytes := tw * 4
	for row := 0; row < th; row++ {
		off := (ty+row)*stride + tx*4
		copy(dst[off:off+rowBytes], src[off:off+rowBytes])
	}
}
