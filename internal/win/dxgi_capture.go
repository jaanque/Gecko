//go:build windows && (amd64 || arm64)

package win

import (
	"errors"
	"image"
	"syscall"
	"unsafe"
)

var (
	d3d11DLL = syscall.NewLazyDLL("d3d11.dll")
	dxgiDLL  = syscall.NewLazyDLL("dxgi.dll")

	procD3D11CreateDevice  = d3d11DLL.NewProc("D3D11CreateDevice")
	procCreateDXGIFactory1 = dxgiDLL.NewProc("CreateDXGIFactory1")
)

const (
	d3dDriverTypeUnknown = 0
	d3d11SDKVersion      = 7
	d3dFeatureLevel11_0  = 0xb000

	d3d11UsageStaging      = 3
	d3d11CPUAccessRead     = 0x20000
	d3d11MapRead           = 1
	dxgiFormatB8G8R8A8Unrm = 87

	dxgiModeRotationUnspecified = 0
	dxgiModeRotationIdentity    = 1

	dxgiErrorNotFound    uint32 = 0x887A0002
	dxgiErrorAccessLost  uint32 = 0x887A0026
	dxgiErrorWaitTimeout uint32 = 0x887A0027

	// Verified 0-based COM vtable indices from Windows SDK (d3d11.h / dxgi.h / dxgi1_2.h).
	vtblIUnknownQueryInterface = 0
	vtblIUnknownRelease        = 2

	vtblIDXGIFactory1EnumAdapters1 = 12
	vtblIDXGIAdapterEnumOutput     = 7

	vtblID3D11DeviceCreateTexture2D = 5

	vtblID3D11DeviceContextMap                   = 14
	vtblID3D11DeviceContextUnmap                 = 15
	vtblID3D11DeviceContextCopySubresourceRegion = 46
	vtblID3D11DeviceContextCopyResource          = 47

	vtblID3D11Texture2DGetDesc = 10

	vtblIDXGIOutputGetDesc          = 7
	vtblIDXGIOutput1DuplicateOutput = 22

	vtblIDXGIOutputDuplicationAcquireNextFrame  = 8
	vtblIDXGIOutputDuplicationGetFrameDirtyRect = 9
	vtblIDXGIOutputDuplicationGetFrameMoveRect  = 10
	vtblIDXGIOutputDuplicationReleaseFrame      = 14
)

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	iidIDXGIFactory1  = guid{0x770aae78, 0xf26f, 0x4dba, [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87}}
	iidIDXGIOutput1   = guid{0x00cddea8, 0x939b, 0x4b83, [8]byte{0xa3, 0x40, 0xa6, 0x85, 0x22, 0x66, 0x66, 0xcc}}
	iidID3D11Texture2 = guid{0x6f15aaf2, 0xd208, 0x4e89, [8]byte{0x9a, 0xb4, 0x48, 0x95, 0x35, 0xd3, 0x4f, 0x9c}}
)

type dxgiOutputDesc struct {
	DeviceName         [32]uint16
	DesktopCoordinates rect
	AttachedToDesktop  int32
	Rotation           uint32
	Monitor            uintptr
}

type dxgiOutduplPointerPosition struct {
	Position point
	Visible  int32
}

type dxgiOutduplFrameInfo struct {
	LastPresentTime           int64
	LastMouseUpdateTime       int64
	AccumulatedFrames         uint32
	RectsCoalesced            int32
	ProtectedContentMaskedOut int32
	PointerPosition           dxgiOutduplPointerPosition
	TotalMetadataBufferSize   uint32
	PointerShapeBufferSize    uint32
}

type dxgiOutduplMoveRect struct {
	SourcePoint     point
	DestinationRect rect
}

type dxgiSampleDesc struct {
	Count   uint32
	Quality uint32
}

type d3d11Texture2DDesc struct {
	Width          uint32
	Height         uint32
	MipLevels      uint32
	ArraySize      uint32
	Format         uint32
	SampleDesc     dxgiSampleDesc
	Usage          uint32
	BindFlags      uint32
	CPUAccessFlags uint32
	MiscFlags      uint32
}

type d3d11MappedSubresource struct {
	PData      uintptr
	RowPitch   uint32
	DepthPitch uint32
}

type d3d11Box struct {
	Left   uint32
	Top    uint32
	Front  uint32
	Right  uint32
	Bottom uint32
	Back   uint32
}

var errDXGIUnsupported = errors.New("dxgi desktop duplication unavailable")

// ptrFromUintptr converts a non-Go C/COM memory address returned by Win32/DXGI into unsafe.Pointer
// without tripping go vet's checkptr warning on raw uintptr->unsafe.Pointer casts.
func ptrFromUintptr(addr uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&addr))
}

// comVtblCall invokes method slot idx on COM object ptr without heap allocations.
func comVtblCall(obj uintptr, idx uintptr, args ...uintptr) uintptr {
	vtbl := *(*uintptr)(ptrFromUintptr(obj))
	fn := *(*uintptr)(unsafe.Add(ptrFromUintptr(vtbl), idx*unsafe.Sizeof(uintptr(0))))
	var callArgs [12]uintptr
	callArgs[0] = obj
	n := 1 + copy(callArgs[1:], args)
	r1, _, _ := syscall.SyscallN(fn, callArgs[:n]...)
	return r1
}

func comRelease(obj uintptr) {
	if obj != 0 {
		comVtblCall(obj, vtblIUnknownRelease)
	}
}

func comQueryInterface(obj uintptr, iid *guid) (uintptr, uint32) {
	var out uintptr
	hr := uint32(comVtblCall(obj, vtblIUnknownQueryInterface, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out))))
	return out, hr
}

// dxgiCapturer implements hardware-accelerated Windows 8/10/11 screen capture via
// IDXGIOutputDuplication across single-GPU and multi-GPU (Optimus/hybrid) setups,
// with GPU dirty/move rectangle extraction, HDR/rotation safety checks, and cursor overlay.
type dxgiCapturer struct {
	device  uintptr
	context uintptr
	dupl    uintptr
	staging uintptr

	originX int
	originY int
	width   int
	height  int
	hasBase bool

	// cleanBGRA holds pure GPU desktop pixels; frameBGRA holds desktop + cursor overlay.
	cleanBGRA []byte
	frameBGRA []byte

	// Reusable metadata buffers to avoid per-frame allocations.
	dirtyBuf []rect
	moveBuf  []dxgiOutduplMoveRect

	// Cursor overlay tracking so cursor-only updates only touch 1-2 tiles.
	curHDC        uintptr
	curBmp        uintptr
	curOld        uintptr
	curBits       []byte
	prevCursorRC  image.Rectangle
	prevCursorPos point
	prevCursorHnd uintptr
}

func (d *dxgiCapturer) Close() {
	d.releaseCursorSurface()
	comRelease(d.staging)
	comRelease(d.dupl)
	comRelease(d.context)
	comRelease(d.device)
	d.staging = 0
	d.dupl = 0
	d.context = 0
	d.device = 0
	d.width = 0
	d.height = 0
	d.hasBase = false
	d.cleanBGRA = nil
	d.frameBGRA = nil
	d.prevCursorRC = image.Rectangle{}
}

// findAdapterAndOutput enumerates every DXGI GPU adapter (supporting hybrid/Optimus laptops
// and multi-GPU desktops) to locate the exact adapter + output matching target monitor bounds.
func findAdapterAndOutput(targetX, targetY, targetW, targetH int) (adapter, output uintptr, desc dxgiOutputDesc, err error) {
	if err := procCreateDXGIFactory1.Find(); err != nil {
		return 0, 0, dxgiOutputDesc{}, errDXGIUnsupported
	}
	var factory uintptr
	hrU, _, _ := procCreateDXGIFactory1.Call(uintptr(unsafe.Pointer(&iidIDXGIFactory1)), uintptr(unsafe.Pointer(&factory)))
	if int32(uint32(hrU)) < 0 || factory == 0 {
		return 0, 0, dxgiOutputDesc{}, errDXGIUnsupported
	}
	defer comRelease(factory)

	for ai := uintptr(0); ai < 16; ai++ {
		var ad uintptr
		hr := uint32(comVtblCall(factory, vtblIDXGIFactory1EnumAdapters1, ai, uintptr(unsafe.Pointer(&ad))))
		if hr == dxgiErrorNotFound || int32(hr) < 0 || ad == 0 {
			break
		}
		for oi := uintptr(0); oi < 16; oi++ {
			var out uintptr
			hr2 := uint32(comVtblCall(ad, vtblIDXGIAdapterEnumOutput, oi, uintptr(unsafe.Pointer(&out))))
			if hr2 == dxgiErrorNotFound || int32(hr2) < 0 || out == 0 {
				break
			}
			var od dxgiOutputDesc
			if hr3 := uint32(comVtblCall(out, vtblIDXGIOutputGetDesc, uintptr(unsafe.Pointer(&od)))); int32(hr3) >= 0 {
				ow := int(od.DesktopCoordinates.Right - od.DesktopCoordinates.Left)
				oh := int(od.DesktopCoordinates.Bottom - od.DesktopCoordinates.Top)
				if od.AttachedToDesktop != 0 &&
					int(od.DesktopCoordinates.Left) == targetX &&
					int(od.DesktopCoordinates.Top) == targetY &&
					ow == targetW && oh == targetH {
					return ad, out, od, nil
				}
			}
			comRelease(out)
		}
		comRelease(ad)
	}
	return 0, 0, dxgiOutputDesc{}, errDXGIUnsupported
}

func (d *dxgiCapturer) initForBounds(targetX, targetY, targetW, targetH int) error {
	if d.dupl != 0 && d.originX == targetX && d.originY == targetY && d.width == targetW && d.height == targetH {
		return nil
	}
	d.Close()

	if err := procD3D11CreateDevice.Find(); err != nil {
		return errDXGIUnsupported
	}

	adapter, matchedOutput, matchedDesc, err := findAdapterAndOutput(targetX, targetY, targetW, targetH)
	if err != nil {
		return err
	}
	defer comRelease(adapter)
	defer comRelease(matchedOutput)

	// Portrait / rotated displays need rotation transforms; delegate those safely to GDI BitBlt.
	if matchedDesc.Rotation != dxgiModeRotationUnspecified && matchedDesc.Rotation != dxgiModeRotationIdentity {
		return errDXGIUnsupported
	}

	featureLevels := [1]uint32{d3dFeatureLevel11_0}
	var chosenLevel uint32
	var dev, ctx uintptr
	// When passing a non-null IDXGIAdapter, D3D11CreateDevice requires D3D_DRIVER_TYPE_UNKNOWN (0).
	hrU, _, _ := procD3D11CreateDevice.Call(
		adapter,
		d3dDriverTypeUnknown,
		0,
		0,
		uintptr(unsafe.Pointer(&featureLevels[0])),
		1,
		d3d11SDKVersion,
		uintptr(unsafe.Pointer(&dev)),
		uintptr(unsafe.Pointer(&chosenLevel)),
		uintptr(unsafe.Pointer(&ctx)),
	)
	if int32(uint32(hrU)) < 0 || dev == 0 || ctx == 0 {
		comRelease(ctx)
		comRelease(dev)
		return errDXGIUnsupported
	}

	output1, hr := comQueryInterface(matchedOutput, &iidIDXGIOutput1)
	if int32(hr) < 0 || output1 == 0 {
		comRelease(ctx)
		comRelease(dev)
		return errDXGIUnsupported
	}
	defer comRelease(output1)

	var dupl uintptr
	hr = uint32(comVtblCall(output1, vtblIDXGIOutput1DuplicateOutput, dev, uintptr(unsafe.Pointer(&dupl))))
	if int32(hr) < 0 || dupl == 0 {
		comRelease(ctx)
		comRelease(dev)
		return errDXGIUnsupported
	}

	w := int(matchedDesc.DesktopCoordinates.Right - matchedDesc.DesktopCoordinates.Left)
	h := int(matchedDesc.DesktopCoordinates.Bottom - matchedDesc.DesktopCoordinates.Top)
	if w <= 0 || h <= 0 || w > 16384 || h > 16384 {
		comRelease(dupl)
		comRelease(ctx)
		comRelease(dev)
		return errDXGIUnsupported
	}

	stagingDesc := d3d11Texture2DDesc{
		Width:          uint32(w),
		Height:         uint32(h),
		MipLevels:      1,
		ArraySize:      1,
		Format:         dxgiFormatB8G8R8A8Unrm,
		SampleDesc:     dxgiSampleDesc{Count: 1, Quality: 0},
		Usage:          d3d11UsageStaging,
		CPUAccessFlags: d3d11CPUAccessRead,
	}
	var staging uintptr
	hr = uint32(comVtblCall(dev, vtblID3D11DeviceCreateTexture2D, uintptr(unsafe.Pointer(&stagingDesc)), 0, uintptr(unsafe.Pointer(&staging))))
	if int32(hr) < 0 || staging == 0 {
		comRelease(dupl)
		comRelease(ctx)
		comRelease(dev)
		return errDXGIUnsupported
	}

	d.device = dev
	d.context = ctx
	d.dupl = dupl
	d.staging = staging
	d.originX = targetX
	d.originY = targetY
	d.width = w
	d.height = h
	d.hasBase = false
	d.cleanBGRA = make([]byte, w*h*4)
	d.frameBGRA = make([]byte, w*h*4)
	d.prevCursorRC = image.Rectangle{}
	return nil
}

// AcquireFrame acquires the next desktop update from the GPU.
// Returns (bgra, dirtyRects, changed, err). When changed==false and err==nil, no pixels changed.
func (d *dxgiCapturer) AcquireFrame(targetX, targetY, targetW, targetH int, forceFull bool) ([]byte, []image.Rectangle, bool, error) {
	if err := d.initForBounds(targetX, targetY, targetW, targetH); err != nil {
		return nil, nil, false, err
	}

	var info dxgiOutduplFrameInfo
	var res uintptr
	timeoutMs := uintptr(16)
	if forceFull && d.hasBase {
		timeoutMs = 0
	}
	hr := uint32(comVtblCall(d.dupl, vtblIDXGIOutputDuplicationAcquireNextFrame, timeoutMs, uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&res))))

	if hr == dxgiErrorWaitTimeout {
		if !d.hasBase {
			return nil, nil, false, nil
		}
		cursorRects, cursorMoved := d.updateCursorOverlay()
		if forceFull {
			return d.frameBGRA, nil, true, nil
		}
		if cursorMoved && len(cursorRects) > 0 {
			return d.frameBGRA, cursorRects, true, nil
		}
		return nil, nil, false, nil
	}

	if hr == dxgiErrorAccessLost || int32(hr) < 0 {
		d.Close()
		return nil, nil, false, errors.New("dxgi access lost")
	}

	defer func() {
		comRelease(res)
		comVtblCall(d.dupl, vtblIDXGIOutputDuplicationReleaseFrame)
	}()

	// If only mouse metadata updated (LastPresentTime == 0) and we already have base frame, skip GPU copy!
	if info.LastPresentTime == 0 && d.hasBase {
		cursorRects, cursorMoved := d.updateCursorOverlay()
		if forceFull {
			return d.frameBGRA, nil, true, nil
		}
		if cursorMoved && len(cursorRects) > 0 {
			return d.frameBGRA, cursorRects, true, nil
		}
		return nil, nil, false, nil
	}

	tex, qhr := comQueryInterface(res, &iidID3D11Texture2)
	if int32(qhr) < 0 || tex == 0 {
		d.Close()
		return nil, nil, false, errors.New("dxgi desktop texture query failed")
	}
	defer comRelease(tex)

	// Guard against HDR (R16G16B16A16_FLOAT) or dynamic resolution mismatches so CopyResource never produces black frames.
	var texDesc d3d11Texture2DDesc
	comVtblCall(tex, vtblID3D11Texture2DGetDesc, uintptr(unsafe.Pointer(&texDesc)))
	if texDesc.Format != dxgiFormatB8G8R8A8Unrm || int(texDesc.Width) != d.width || int(texDesc.Height) != d.height {
		d.Close()
		return nil, nil, false, errDXGIUnsupported
	}

	var dirtyRects []image.Rectangle
	needFullCopy := !d.hasBase || forceFull || info.TotalMetadataBufferSize == 0
	if !needFullCopy {
		dirtyRects = d.extractDirtyRects(info.TotalMetadataBufferSize)
		if len(dirtyRects) == 0 {
			needFullCopy = true
		}
	}

	if needFullCopy {
		comVtblCall(d.context, vtblID3D11DeviceContextCopyResource, d.staging, tex)
	} else {
		for _, r := range dirtyRects {
			box := d3d11Box{
				Left:   uint32(r.Min.X),
				Top:    uint32(r.Min.Y),
				Front:  0,
				Right:  uint32(r.Max.X),
				Bottom: uint32(r.Max.Y),
				Back:   1,
			}
			comVtblCall(
				d.context,
				vtblID3D11DeviceContextCopySubresourceRegion,
				d.staging, 0,
				uintptr(box.Left), uintptr(box.Top), 0,
				tex, 0,
				uintptr(unsafe.Pointer(&box)),
			)
		}
	}

	var mapped d3d11MappedSubresource
	mhr := uint32(comVtblCall(d.context, vtblID3D11DeviceContextMap, d.staging, 0, d3d11MapRead, 0, uintptr(unsafe.Pointer(&mapped))))
	if int32(mhr) < 0 || mapped.PData == 0 {
		d.Close()
		return nil, nil, false, errors.New("d3d11 staging Map failed")
	}

	pitch := int(mapped.RowPitch)
	srcTotal := pitch * d.height
	srcBytes := unsafe.Slice((*byte)(ptrFromUintptr(mapped.PData)), srcTotal)
	dstStride := d.width * 4

	if needFullCopy {
		for y := 0; y < d.height; y++ {
			srcOff := y * pitch
			dstOff := y * dstStride
			copy(d.cleanBGRA[dstOff:dstOff+dstStride], srcBytes[srcOff:srcOff+dstStride])
		}
		copy(d.frameBGRA, d.cleanBGRA)
		d.hasBase = true
		d.prevCursorRC = image.Rectangle{}
		dirtyRects = nil
	} else {
		for _, r := range dirtyRects {
			rowBytes := r.Dx() * 4
			for y := r.Min.Y; y < r.Max.Y; y++ {
				srcOff := y*pitch + r.Min.X*4
				dstOff := y*dstStride + r.Min.X*4
				copy(d.cleanBGRA[dstOff:dstOff+rowBytes], srcBytes[srcOff:srcOff+rowBytes])
				copy(d.frameBGRA[dstOff:dstOff+rowBytes], srcBytes[srcOff:srcOff+rowBytes])
			}
		}
	}

	comVtblCall(d.context, vtblID3D11DeviceContextUnmap, d.staging, 0)

	cursorRects, _ := d.updateCursorOverlay()
	if !needFullCopy && len(cursorRects) > 0 {
		dirtyRects = append(dirtyRects, cursorRects...)
	}

	return d.frameBGRA, dirtyRects, true, nil
}

func (d *dxgiCapturer) extractDirtyRects(metaBytes uint32) []image.Rectangle {
	screen := image.Rect(0, 0, d.width, d.height)
	var out []image.Rectangle

	maxMoves := int(metaBytes)/int(unsafe.Sizeof(dxgiOutduplMoveRect{})) + 1
	if cap(d.moveBuf) < maxMoves {
		d.moveBuf = make([]dxgiOutduplMoveRect, maxMoves)
	} else {
		d.moveBuf = d.moveBuf[:maxMoves]
	}
	var moveBytes uint32
	hr := uint32(comVtblCall(
		d.dupl,
		vtblIDXGIOutputDuplicationGetFrameMoveRect,
		uintptr(uint32(len(d.moveBuf)*int(unsafe.Sizeof(dxgiOutduplMoveRect{})))),
		uintptr(unsafe.Pointer(&d.moveBuf[0])),
		uintptr(unsafe.Pointer(&moveBytes)),
	))
	if int32(hr) >= 0 && moveBytes > 0 {
		count := int(moveBytes) / int(unsafe.Sizeof(dxgiOutduplMoveRect{}))
		for i := 0; i < count && i < len(d.moveBuf); i++ {
			dr := d.moveBuf[i].DestinationRect
			r := image.Rect(int(dr.Left), int(dr.Top), int(dr.Right), int(dr.Bottom)).Intersect(screen)
			if !r.Empty() {
				out = append(out, r)
			}
		}
	}

	maxDirty := int(metaBytes)/int(unsafe.Sizeof(rect{})) + 1
	if cap(d.dirtyBuf) < maxDirty {
		d.dirtyBuf = make([]rect, maxDirty)
	} else {
		d.dirtyBuf = d.dirtyBuf[:maxDirty]
	}
	var dirtyBytes uint32
	hr = uint32(comVtblCall(
		d.dupl,
		vtblIDXGIOutputDuplicationGetFrameDirtyRect,
		uintptr(uint32(len(d.dirtyBuf)*int(unsafe.Sizeof(rect{})))),
		uintptr(unsafe.Pointer(&d.dirtyBuf[0])),
		uintptr(unsafe.Pointer(&dirtyBytes)),
	))
	if int32(hr) >= 0 && dirtyBytes > 0 {
		count := int(dirtyBytes) / int(unsafe.Sizeof(rect{}))
		for i := 0; i < count && i < len(d.dirtyBuf); i++ {
			dr := d.dirtyBuf[i]
			r := image.Rect(int(dr.Left), int(dr.Top), int(dr.Right), int(dr.Bottom)).Intersect(screen)
			if !r.Empty() {
				out = append(out, r)
			}
		}
	}

	return out
}

const cursorPatchSize = 64

func (d *dxgiCapturer) ensureCursorSurface() bool {
	if d.curHDC != 0 && d.curBmp != 0 && len(d.curBits) == cursorPatchSize*cursorPatchSize*4 {
		return true
	}
	d.releaseCursorSurface()

	hdcScreen, _, _ := procGetDC.Call(0)
	if hdcScreen == 0 {
		return false
	}
	defer procReleaseDC.Call(0, hdcScreen)

	memDC, _, _ := procCreateCompatibleDC.Call(hdcScreen)
	if memDC == 0 {
		return false
	}

	var bi bitmapInfo
	bi.Header.Size = uint32(unsafe.Sizeof(bi.Header))
	bi.Header.Width = cursorPatchSize
	bi.Header.Height = -cursorPatchSize
	bi.Header.Planes = 1
	bi.Header.BitCount = 32
	bi.Header.Compression = biRGB

	var bits unsafe.Pointer
	hBmp, _, _ := procCreateDIBSection.Call(
		memDC,
		uintptr(unsafe.Pointer(&bi)),
		dibRGBColors,
		uintptr(unsafe.Pointer(&bits)),
		0, 0,
	)
	if hBmp == 0 || bits == nil {
		procDeleteDC.Call(memDC)
		return false
	}
	oldObj, _, _ := procSelectObject.Call(memDC, hBmp)
	d.curHDC = memDC
	d.curBmp = hBmp
	d.curOld = oldObj
	d.curBits = unsafe.Slice((*byte)(bits), cursorPatchSize*cursorPatchSize*4)
	return true
}

func (d *dxgiCapturer) releaseCursorSurface() {
	if d.curHDC != 0 && d.curOld != 0 {
		procSelectObject.Call(d.curHDC, d.curOld)
		d.curOld = 0
	}
	if d.curBmp != 0 {
		procDeleteObject.Call(d.curBmp)
		d.curBmp = 0
	}
	if d.curHDC != 0 {
		procDeleteDC.Call(d.curHDC)
		d.curHDC = 0
	}
	d.curBits = nil
}

func (d *dxgiCapturer) updateCursorOverlay() ([]image.Rectangle, bool) {
	if !d.hasBase {
		return nil, false
	}

	var ci cursorInfo
	ci.CbSize = uint32(unsafe.Sizeof(ci))
	r, _, _ := procGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci)))
	visible := r != 0 && (ci.Flags&cursorShowing) != 0 && ci.HCursor != 0

	var newRC image.Rectangle
	var drawX, drawY int
	if visible {
		drawX = int(ci.PtScreenPos.X) - d.originX
		drawY = int(ci.PtScreenPos.Y) - d.originY
		var ii iconInfo
		if ok, _, _ := procGetIconInfo.Call(ci.HCursor, uintptr(unsafe.Pointer(&ii))); ok != 0 {
			drawX -= int(ii.XHotspot)
			drawY -= int(ii.YHotspot)
			if ii.HbmMask != 0 {
				procDeleteObject.Call(ii.HbmMask)
			}
			if ii.HbmColor != 0 {
				procDeleteObject.Call(ii.HbmColor)
			}
		}
		screen := image.Rect(0, 0, d.width, d.height)
		newRC = image.Rect(drawX, drawY, drawX+cursorPatchSize, drawY+cursorPatchSize).Intersect(screen)
	}

	moved := ci.PtScreenPos != d.prevCursorPos || ci.HCursor != d.prevCursorHnd || newRC != d.prevCursorRC

	var dirty []image.Rectangle
	stride := d.width * 4

	if !d.prevCursorRC.Empty() {
		rowBytes := d.prevCursorRC.Dx() * 4
		for y := d.prevCursorRC.Min.Y; y < d.prevCursorRC.Max.Y; y++ {
			off := y*stride + d.prevCursorRC.Min.X*4
			copy(d.frameBGRA[off:off+rowBytes], d.cleanBGRA[off:off+rowBytes])
		}
		if moved {
			dirty = append(dirty, d.prevCursorRC)
		}
	}

	if !newRC.Empty() && d.ensureCursorSurface() {
		pw := newRC.Dx()
		ph := newRC.Dy()
		clear(d.curBits)
		for y := 0; y < ph; y++ {
			srcOff := (newRC.Min.Y+y)*stride + newRC.Min.X*4
			dstOff := y * cursorPatchSize * 4
			copy(d.curBits[dstOff:dstOff+pw*4], d.cleanBGRA[srcOff:srcOff+pw*4])
		}
		procDrawIconEx.Call(
			d.curHDC,
			uintptr(int32(drawX-newRC.Min.X)),
			uintptr(int32(drawY-newRC.Min.Y)),
			ci.HCursor, 0, 0, 0, 0, diNormal,
		)
		for y := 0; y < ph; y++ {
			srcOff := y * cursorPatchSize * 4
			dstOff := (newRC.Min.Y+y)*stride + newRC.Min.X*4
			copy(d.frameBGRA[dstOff:dstOff+pw*4], d.curBits[srcOff:srcOff+pw*4])
		}
		if moved {
			dirty = append(dirty, newRC)
		}
	}

	d.prevCursorRC = newRC
	d.prevCursorPos = ci.PtScreenPos
	d.prevCursorHnd = ci.HCursor
	return dirty, moved
}
