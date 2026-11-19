//go:build windows

package win

import (
	"bytes"
	"image/color"
	"image/png"
	"syscall"
	"unsafe"
)

var (
	msimg32 = syscall.NewLazyDLL("msimg32.dll")

	procAlphaBlend         = msimg32.NewProc("AlphaBlend")
	procCreateDIBSection   = gdi32GUI.NewProc("CreateDIBSection")
	procCreateBitmap       = gdi32GUI.NewProc("CreateBitmap")
	procCreateIconIndirect = user32GUI.NewProc("CreateIconIndirect")
	procDestroyIcon        = user32GUI.NewProc("DestroyIcon")
)

const (
	wmSetIcon = 0x0080
	iconSmall = 0
	iconBig   = 1

	// BLENDFUNCTION { AC_SRC_OVER(0), 0, 255, AC_SRC_ALPHA(1) } packed little-endian.
	blendFuncAlpha = 0x01FF0000
)

type logoGDI struct {
	hBitmap uintptr
	hIcon   uintptr
	width   int32
	height  int32
}

func newLogoGDI(pngBytes []byte) *logoGDI {
	if len(pngBytes) == 0 {
		return nil
	}
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 || w > 512 || h > 512 {
		return nil
	}

	var bi bitmapInfo
	bi.Header.Size = uint32(unsafe.Sizeof(bi.Header))
	bi.Header.Width = int32(w)
	bi.Header.Height = -int32(h) // top-down
	bi.Header.Planes = 1
	bi.Header.BitCount = 32
	bi.Header.Compression = biRGB

	var bits unsafe.Pointer
	hBmp, _, _ := procCreateDIBSection.Call(
		0,
		uintptr(unsafe.Pointer(&bi)),
		dibRGBColors,
		uintptr(unsafe.Pointer(&bits)),
		0, 0,
	)
	if hBmp == 0 || bits == nil {
		return nil
	}

	pix := unsafe.Slice((*byte)(bits), w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.NRGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			a := uint32(c.A)
			// Premultiplied BGRA for Win32 AlphaBlend and 32-bit HICON.
			off := (y*w + x) * 4
			pix[off+0] = byte((uint32(c.B) * a) / 255)
			pix[off+1] = byte((uint32(c.G) * a) / 255)
			pix[off+2] = byte((uint32(c.R) * a) / 255)
			pix[off+3] = c.A
		}
	}

	// Build native HICON for titlebar & taskbar.
	var hIcon uintptr
	hMask, _, _ := procCreateBitmap.Call(uintptr(w), uintptr(h), 1, 1, 0)
	if hMask != 0 {
		ii := iconInfo{
			FIcon:    1,
			HbmMask:  hMask,
			HbmColor: hBmp,
		}
		hIcon, _, _ = procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))
		procDeleteObject.Call(hMask)
	}

	return &logoGDI{
		hBitmap: hBmp,
		hIcon:   hIcon,
		width:   int32(w),
		height:  int32(h),
	}
}

func (l *logoGDI) DrawFit(hdc uintptr, box rect) {
	if l == nil || l.hBitmap == 0 || l.width <= 0 || l.height <= 0 {
		return
	}
	bw := box.Right - box.Left
	bh := box.Bottom - box.Top
	if bw <= 0 || bh <= 0 {
		return
	}

	dstW := bw
	dstH := int32(int64(bw) * int64(l.height) / int64(l.width))
	if dstH > bh {
		dstH = bh
		dstW = int32(int64(bh) * int64(l.width) / int64(l.height))
	}
	dstX := box.Left + (bw-dstW)/2
	dstY := box.Top + (bh-dstH)/2

	srcDC, _, _ := procCreateCompatibleDC.Call(hdc)
	if srcDC == 0 {
		return
	}
	oldBmp, _, _ := procSelectObject.Call(srcDC, l.hBitmap)
	procAlphaBlend.Call(
		hdc,
		uintptr(dstX), uintptr(dstY), uintptr(dstW), uintptr(dstH),
		srcDC,
		0, 0, uintptr(l.width), uintptr(l.height),
		blendFuncAlpha,
	)
	procSelectObject.Call(srcDC, oldBmp)
	procDeleteDC.Call(srcDC)
}

func (l *logoGDI) Close() {
	if l == nil {
		return
	}
	if l.hIcon != 0 {
		procDestroyIcon.Call(l.hIcon)
		l.hIcon = 0
	}
	if l.hBitmap != 0 {
		procDeleteObject.Call(l.hBitmap)
		l.hBitmap = 0
	}
}
