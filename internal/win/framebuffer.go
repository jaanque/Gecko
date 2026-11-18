package win

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"runtime"
	"sync"
	"sync/atomic"

	"remote-control/internal/protocol"
)

// FrameMeta holds header information extracted from a decoded MsgFrame.
type FrameMeta struct {
	FrameID      uint32
	SentUnixNano int64
	Width        int
	Height       int
}

// FrameBuffer stores the remote desktop image in top-down 32-bit BGRA format
// ready for direct Win32 StretchDIBits rendering.
type FrameBuffer struct {
	mu     sync.RWMutex
	width  int
	height int
	bgra   []byte
}

// NewFrameBuffer creates an empty FrameBuffer.
func NewFrameBuffer() *FrameBuffer {
	return &FrameBuffer{}
}

// Reset clears the framebuffer and prepares dimensions.
func (fb *FrameBuffer) Reset(width, height int) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if width <= 0 || height <= 0 {
		fb.width = 0
		fb.height = 0
		fb.bgra = nil
		return
	}
	fb.width = width
	fb.height = height
	needed := width * height * 4
	if cap(fb.bgra) < needed {
		fb.bgra = make([]byte, needed)
	} else {
		fb.bgra = fb.bgra[:needed]
		clear(fb.bgra)
	}
}

type rawTile struct {
	x, y, w, h int
	codec      uint8
	payload    []byte
}

type decodedTileResult struct {
	x, y, w, h int
	img        image.Image
	bgr        []byte
}

// ApplyFrame decodes a binary MsgFrame payload (hybrid JPEG + lossless Zlib-BGR tiles)
// in parallel and applies all tiles atomically into the BGRA framebuffer.
func (fb *FrameBuffer) ApplyFrame(data []byte) (FrameMeta, error) {
	if len(data) < protocol.FrameHeaderSize {
		return FrameMeta{}, errors.New("frame header too short")
	}
	meta := FrameMeta{
		FrameID:      binary.BigEndian.Uint32(data[0:4]),
		SentUnixNano: int64(binary.BigEndian.Uint64(data[4:12])),
		Width:        int(binary.BigEndian.Uint16(data[12:14])),
		Height:       int(binary.BigEndian.Uint16(data[14:16])),
	}
	tileCount := int(binary.BigEndian.Uint16(data[16:18]))
	if meta.Width <= 0 || meta.Height <= 0 || meta.Width > 16384 || meta.Height > 16384 {
		return FrameMeta{}, errors.New("invalid screen dimensions in frame")
	}
	if tileCount == 0 {
		return meta, nil
	}

	rawTiles := make([]rawTile, 0, tileCount)
	off := protocol.FrameHeaderSize
	for i := 0; i < tileCount; i++ {
		if off+protocol.TileHeaderSize > len(data) {
			break
		}
		tx := int(binary.BigEndian.Uint16(data[off : off+2]))
		ty := int(binary.BigEndian.Uint16(data[off+2 : off+4]))
		tw := int(binary.BigEndian.Uint16(data[off+4 : off+6]))
		th := int(binary.BigEndian.Uint16(data[off+6 : off+8]))
		codec := data[off+8]
		dlen := int(binary.BigEndian.Uint32(data[off+9 : off+13]))
		off += protocol.TileHeaderSize
		if dlen <= 0 || off+dlen > len(data) {
			break
		}
		rawTiles = append(rawTiles, rawTile{
			x: tx, y: ty, w: tw, h: th,
			codec:   codec,
			payload: data[off : off+dlen],
		})
		off += dlen
	}
	if len(rawTiles) == 0 {
		return meta, nil
	}

	decoded := make([]decodedTileResult, len(rawTiles))
	if len(rawTiles) <= 3 {
		for i, rt := range rawTiles {
			decoded[i] = decodeRawTile(rt)
		}
	} else {
		workers := runtime.NumCPU()
		if workers > 8 {
			workers = 8
		}
		if workers > len(rawTiles) {
			workers = len(rawTiles)
		}
		var nextIdx atomic.Int64
		var wg sync.WaitGroup
		wg.Add(workers)
		for w := 0; w < workers; w++ {
			go func() {
				defer wg.Done()
				for {
					idx := int(nextIdx.Add(1) - 1)
					if idx >= len(rawTiles) {
						return
					}
					decoded[idx] = decodeRawTile(rawTiles[idx])
				}
			}()
		}
		wg.Wait()
	}

	fb.mu.Lock()
	defer fb.mu.Unlock()

	if fb.width != meta.Width || fb.height != meta.Height || len(fb.bgra) != meta.Width*meta.Height*4 {
		fb.width = meta.Width
		fb.height = meta.Height
		needed := meta.Width * meta.Height * 4
		if cap(fb.bgra) < needed {
			fb.bgra = make([]byte, needed)
		} else {
			fb.bgra = fb.bgra[:needed]
			clear(fb.bgra)
		}
	}

	for _, d := range decoded {
		if d.bgr != nil {
			blitBGRToBGRA(fb.bgra, fb.width, fb.height, d.x, d.y, d.w, d.h, d.bgr)
		} else if d.img != nil {
			blitImageToBGRA(fb.bgra, fb.width, fb.height, d.x, d.y, d.img)
		}
	}
	return meta, nil
}

func decodeRawTile(rt rawTile) decodedTileResult {
	switch rt.codec {
	case protocol.TileCodecZlibBGR:
		zr, err := zlib.NewReader(bytes.NewReader(rt.payload))
		if err != nil {
			return decodedTileResult{}
		}
		expected := rt.w * rt.h * 3
		if expected <= 0 || expected > 256*256*3 {
			_ = zr.Close()
			return decodedTileResult{}
		}
		bgr := make([]byte, expected)
		_, err = io.ReadFull(zr, bgr)
		_ = zr.Close()
		if err != nil {
			return decodedTileResult{}
		}
		return decodedTileResult{x: rt.x, y: rt.y, w: rt.w, h: rt.h, bgr: bgr}

	case protocol.TileCodecJPEG:
		img, err := jpeg.Decode(bytes.NewReader(rt.payload))
		if err != nil {
			return decodedTileResult{}
		}
		return decodedTileResult{x: rt.x, y: rt.y, w: rt.w, h: rt.h, img: img}

	default:
		return decodedTileResult{}
	}
}

// WithReadLock executes fn while holding the read lock on the BGRA buffer.
func (fb *FrameBuffer) WithReadLock(fn func(width, height int, bgra []byte)) {
	fb.mu.RLock()
	defer fb.mu.RUnlock()
	fn(fb.width, fb.height, fb.bgra)
}

func blitBGRToBGRA(dst []byte, dstW, dstH, tx, ty, tw, th int, src []byte) {
	if tx >= dstW || ty >= dstH || tw <= 0 || th <= 0 {
		return
	}
	w := tw
	h := th
	if tx+w > dstW {
		w = dstW - tx
	}
	if ty+h > dstH {
		h = dstH - ty
	}
	stride := dstW * 4
	srcStride := tw * 3
	for y := 0; y < h; y++ {
		srcRow := src[y*srcStride : y*srcStride+w*3]
		dstOff := (ty+y)*stride + tx*4
		s := 0
		for x := 0; x < w; x++ {
			o := dstOff + x*4
			dst[o+0] = srcRow[s+0]
			dst[o+1] = srcRow[s+1]
			dst[o+2] = srcRow[s+2]
			dst[o+3] = 255
			s += 3
		}
	}
}

func blitImageToBGRA(dst []byte, dstW, dstH, tx, ty int, src image.Image) {
	b := src.Bounds()
	w := b.Dx()
	h := b.Dy()
	if tx >= dstW || ty >= dstH || w <= 0 || h <= 0 {
		return
	}
	if tx+w > dstW {
		w = dstW - tx
	}
	if ty+h > dstH {
		h = dstH - ty
	}
	stride := dstW * 4

	switch img := src.(type) {
	case *image.YCbCr:
		for y := 0; y < h; y++ {
			sy := b.Min.Y + y
			yi := img.YOffset(b.Min.X, sy)
			ci := img.COffset(b.Min.X, sy)
			dstOff := (ty+y)*stride + tx*4

			switch img.SubsampleRatio {
			case image.YCbCrSubsampleRatio420, image.YCbCrSubsampleRatio422:
				for x := 0; x < w; x++ {
					yy := img.Y[yi+x]
					cb := img.Cb[ci+(x>>1)]
					cr := img.Cr[ci+(x>>1)]
					r, g, bl := color.YCbCrToRGB(yy, cb, cr)
					o := dstOff + x*4
					dst[o+0] = bl
					dst[o+1] = g
					dst[o+2] = r
					dst[o+3] = 255
				}
			case image.YCbCrSubsampleRatio444:
				for x := 0; x < w; x++ {
					r, g, bl := color.YCbCrToRGB(img.Y[yi+x], img.Cb[ci+x], img.Cr[ci+x])
					o := dstOff + x*4
					dst[o+0] = bl
					dst[o+1] = g
					dst[o+2] = r
					dst[o+3] = 255
				}
			default:
				for x := 0; x < w; x++ {
					sx := b.Min.X + x
					yy := img.Y[img.YOffset(sx, sy)]
					co := img.COffset(sx, sy)
					r, g, bl := color.YCbCrToRGB(yy, img.Cb[co], img.Cr[co])
					o := dstOff + x*4
					dst[o+0] = bl
					dst[o+1] = g
					dst[o+2] = r
					dst[o+3] = 255
				}
			}
		}

	case *image.RGBA:
		for y := 0; y < h; y++ {
			srcOff := (b.Min.Y+y-img.Rect.Min.Y)*img.Stride + (b.Min.X-img.Rect.Min.X)*4
			dstOff := (ty+y)*stride + tx*4
			for x := 0; x < w; x++ {
				so := srcOff + x*4
				do := dstOff + x*4
				dst[do+0] = img.Pix[so+2]
				dst[do+1] = img.Pix[so+1]
				dst[do+2] = img.Pix[so+0]
				dst[do+3] = 255
			}
		}

	default:
		for y := 0; y < h; y++ {
			dstOff := (ty+y)*stride + tx*4
			for x := 0; x < w; x++ {
				r, g, bl, _ := src.At(b.Min.X+x, b.Min.Y+y).RGBA()
				do := dstOff + x*4
				dst[do+0] = byte(bl >> 8)
				dst[do+1] = byte(g >> 8)
				dst[do+2] = byte(r >> 8)
				dst[do+3] = 255
			}
		}
	}
}
