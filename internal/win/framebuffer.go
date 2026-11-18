package win

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"sync"
)

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

// ApplyFrame decodes a binary MsgFrame payload (dirty JPEG tiles) directly into the BGRA buffer.
func (fb *FrameBuffer) ApplyFrame(data []byte) (int, int, error) {
	if len(data) < 6 {
		return 0, 0, errors.New("frame header too short")
	}
	screenW := int(binary.BigEndian.Uint16(data[0:2]))
	screenH := int(binary.BigEndian.Uint16(data[2:4]))
	tileCount := int(binary.BigEndian.Uint16(data[4:6]))
	if screenW <= 0 || screenH <= 0 || screenW > 16384 || screenH > 16384 {
		return 0, 0, errors.New("invalid screen dimensions in frame")
	}
	if tileCount == 0 {
		return screenW, screenH, nil
	}

	type decodedTile struct {
		x, y int
		img  image.Image
	}
	tiles := make([]decodedTile, 0, tileCount)

	off := 6
	for i := 0; i < tileCount; i++ {
		if off+12 > len(data) {
			break
		}
		tx := int(binary.BigEndian.Uint16(data[off : off+2]))
		ty := int(binary.BigEndian.Uint16(data[off+2 : off+4]))
		// tw, th at off+4..off+8
		jpgLen := int(binary.BigEndian.Uint32(data[off+8 : off+12]))
		off += 12
		if jpgLen <= 0 || off+jpgLen > len(data) {
			break
		}
		img, err := jpeg.Decode(bytes.NewReader(data[off : off+jpgLen]))
		off += jpgLen
		if err != nil {
			continue
		}
		tiles = append(tiles, decodedTile{x: tx, y: ty, img: img})
	}

	if len(tiles) == 0 {
		return screenW, screenH, nil
	}

	fb.mu.Lock()
	defer fb.mu.Unlock()

	if fb.width != screenW || fb.height != screenH || len(fb.bgra) != screenW*screenH*4 {
		fb.width = screenW
		fb.height = screenH
		needed := screenW * screenH * 4
		if cap(fb.bgra) < needed {
			fb.bgra = make([]byte, needed)
		} else {
			fb.bgra = fb.bgra[:needed]
			clear(fb.bgra)
		}
	}

	for _, t := range tiles {
		blitImageToBGRA(fb.bgra, fb.width, fb.height, t.x, t.y, t.img)
	}
	return screenW, screenH, nil
}

// WithReadLock executes fn while holding the read lock on the BGRA buffer.
func (fb *FrameBuffer) WithReadLock(fn func(width, height int, bgra []byte)) {
	fb.mu.RLock()
	defer fb.mu.RUnlock()
	fn(fb.width, fb.height, fb.bgra)
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
