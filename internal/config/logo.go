package config

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"os"
)

const (
	maxLogoDim       = 128
	maxLogoInputSize = 10 * 1024 * 1024 // 10 MiB max source image
)

// LoadAndNormalizeLogo reads a PNG or JPEG image file, scales it cleanly to fit within 128x128
// preserving aspect ratio and alpha transparency, and returns compact PNG bytes.
func LoadAndNormalizeLogo(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxLogoInputSize {
		return nil, errors.New("la imagen del logo supera el límite de 10 MB")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return NormalizeLogoBytes(raw)
}

// NormalizeLogoBytes decodes PNG/JPEG bytes, resizes to fit maxLogoDim, and encodes as PNG.
func NormalizeLogoBytes(raw []byte) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("formato de imagen no compatible (usa PNG o JPG): %w", err)
	}

	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return nil, errors.New("imagen de logo vacía")
	}

	dw, dh := sw, sh
	if sw > maxLogoDim || sh > maxLogoDim {
		if sw >= sh {
			dw = maxLogoDim
			dh = max(1, (sh*maxLogoDim)/sw)
		} else {
			dh = maxLogoDim
			dw = max(1, (sw*maxLogoDim)/sh)
		}
	}

	dst := resizeNRGBA(src, dw, dh)
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, dst); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// resizeNRGBA performs crisp area-averaged / bilinear scaling into NRGBA preserving alpha.
func resizeNRGBA(src image.Image, dstW, dstH int) *image.NRGBA {
	b := src.Bounds()
	srcW, srcH := b.Dx(), b.Dy()

	// Convert source to NRGBA once for fast direct access.
	srcN := image.NewNRGBA(image.Rect(0, 0, srcW, srcH))
	for y := 0; y < srcH; y++ {
		for x := 0; x < srcW; x++ {
			srcN.Set(x, y, color.NRGBAModel.Convert(src.At(b.Min.X+x, b.Min.Y+y)))
		}
	}
	if srcW == dstW && srcH == dstH {
		return srcN
	}

	dst := image.NewNRGBA(image.Rect(0, 0, dstW, dstH))
	scaleX := float64(srcW) / float64(dstW)
	scaleY := float64(srcH) / float64(dstH)

	for y := 0; y < dstH; y++ {
		y0 := int(float64(y) * scaleY)
		y1 := int(float64(y+1) * scaleY)
		if y1 <= y0 {
			y1 = y0 + 1
		}
		if y1 > srcH {
			y1 = srcH
		}

		for x := 0; x < dstW; x++ {
			x0 := int(float64(x) * scaleX)
			x1 := int(float64(x+1) * scaleX)
			if x1 <= x0 {
				x1 = x0 + 1
			}
			if x1 > srcW {
				x1 = srcW
			}

			var sumR, sumG, sumB, sumA, count uint64
			for sy := y0; sy < y1; sy++ {
				rowOff := sy * srcN.Stride
				for sx := x0; sx < x1; sx++ {
					o := rowOff + sx*4
					a := uint64(srcN.Pix[o+3])
					sumR += uint64(srcN.Pix[o+0]) * a
					sumG += uint64(srcN.Pix[o+1]) * a
					sumB += uint64(srcN.Pix[o+2]) * a
					sumA += a
					count++
				}
			}

			do := y*dst.Stride + x*4
			if sumA > 0 {
				dst.Pix[do+0] = uint8(sumR / sumA)
				dst.Pix[do+1] = uint8(sumG / sumA)
				dst.Pix[do+2] = uint8(sumB / sumA)
				dst.Pix[do+3] = uint8(sumA / count)
			}
		}
	}
	return dst
}
