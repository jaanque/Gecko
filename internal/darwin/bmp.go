package darwin

import (
	"encoding/binary"
	"errors"
)

// decodeBMPToBGRA decodes an uncompressed 24-bit or 32-bit Windows BMP (produced by macOS screencapture -t bmp)
// into a top-down 32-bit BGRA buffer suitable for video.TileEncoder.
func decodeBMPToBGRA(raw, dst []byte) ([]byte, int, int, error) {
	if len(raw) < 54 || raw[0] != 'B' || raw[1] != 'M' {
		return dst, 0, 0, errors.New("invalid BMP header")
	}
	pixOff := int(binary.LittleEndian.Uint32(raw[10:14]))
	w := int(int32(binary.LittleEndian.Uint32(raw[18:22])))
	rawH := int(int32(binary.LittleEndian.Uint32(raw[22:26])))
	bpp := int(binary.LittleEndian.Uint16(raw[28:30]))
	comp := binary.LittleEndian.Uint32(raw[30:34])
	if w <= 0 || rawH == 0 || (bpp != 24 && bpp != 32) || (comp != 0 && comp != 3) {
		return dst, 0, 0, errors.New("unsupported BMP format")
	}
	topDown := rawH < 0
	h := rawH
	if topDown {
		h = -rawH
	}
	if w > 16384 || h > 16384 {
		return dst, 0, 0, errors.New("BMP dimensions too large")
	}

	bytesPerPx := bpp / 8
	srcStride := ((w * bytesPerPx) + 3) &^ 3
	if pixOff < 54 || pixOff+srcStride*h > len(raw) {
		return dst, 0, 0, errors.New("truncated BMP data")
	}

	needed := w * h * 4
	if cap(dst) < needed {
		dst = make([]byte, needed)
	} else {
		dst = dst[:needed]
	}

	dstStride := w * 4
	for y := 0; y < h; y++ {
		srcY := y
		if !topDown {
			srcY = h - 1 - y
		}
		srcRow := raw[pixOff+srcY*srcStride : pixOff+srcY*srcStride+w*bytesPerPx]
		dstRow := dst[y*dstStride : (y+1)*dstStride]
		if bytesPerPx == 4 {
			copy(dstRow, srcRow)
			for i := 3; i < len(dstRow); i += 4 {
				dstRow[i] = 255
			}
		} else {
			s := 0
			for d := 0; d < len(dstRow); d += 4 {
				dstRow[d+0] = srcRow[s+0]
				dstRow[d+1] = srcRow[s+1]
				dstRow[d+2] = srcRow[s+2]
				dstRow[d+3] = 255
				s += 3
			}
		}
	}
	return dst, w, h, nil
}
