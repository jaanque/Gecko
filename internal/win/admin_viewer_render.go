//go:build windows

package win

import (
	"time"
	"unsafe"
)

var (
	procSetStretchBltMode = gdi32GUI.NewProc("SetStretchBltMode")
	procSetBrushOrgEx     = gdi32GUI.NewProc("SetBrushOrgEx")
	procStretchDIBits     = gdi32GUI.NewProc("StretchDIBits")
)

const (
	colorOnColorMode = 3
	halftoneMode     = 4
)

func (v *remoteViewer) ensureBackbuffer(hdc uintptr, w, h int32) uintptr {
	if v.backDC != 0 && v.backBmp != 0 && v.backW == w && v.backH == h {
		return v.backDC
	}
	v.releaseBackbuffer()
	memDC, _, _ := procCreateCompatibleDC.Call(hdc)
	memBmp, _, _ := procCreateCompatibleBitmap.Call(hdc, uintptr(w), uintptr(h))
	oldBmp, _, _ := procSelectObject.Call(memDC, memBmp)
	v.backDC = memDC
	v.backBmp = memBmp
	v.backOld = oldBmp
	v.backW = w
	v.backH = h
	return v.backDC
}

func (v *remoteViewer) releaseBackbuffer() {
	if v.backDC != 0 && v.backOld != 0 {
		procSelectObject.Call(v.backDC, v.backOld)
		v.backOld = 0
	}
	if v.backBmp != 0 {
		procDeleteObject.Call(v.backBmp)
		v.backBmp = 0
	}
	if v.backDC != 0 {
		procDeleteDC.Call(v.backDC)
		v.backDC = 0
	}
	v.backW = 0
	v.backH = 0
}

func (v *remoteViewer) decodeLoop(sessionID string, ch <-chan []byte, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case data, ok := <-ch:
			if !ok {
				return
			}
			meta, err := v.fb.ApplyFrame(data)
			if err != nil {
				continue
			}

			if v.cb.OnFrameAck != nil && meta.FrameID > 0 {
				v.cb.OnFrameAck(meta.FrameID)
			}

			v.mu.Lock()
			if v.sessionID != sessionID {
				v.mu.Unlock()
				return
			}
			v.remoteW = meta.Width
			v.remoteH = meta.Height
			v.fpsCount++
			now := time.Now()
			if elapsed := now.Sub(v.fpsTick); elapsed >= time.Second {
				v.fps = int(float64(v.fpsCount)/elapsed.Seconds() + 0.5)
				v.fpsCount = 0
				v.fpsTick = now
			}
			hwnd := v.hwnd
			v.mu.Unlock()

			if hwnd != 0 {
				procPostMessageW.Call(hwnd, wmViewerFrame, 0, 0)
			}
		}
	}
}

func (v *remoteViewer) paint(hwnd uintptr) {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 {
		return
	}
	defer procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))

	var cr rect
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&cr)))
	cw := cr.Right - cr.Left
	ch := cr.Bottom - cr.Top
	if cw <= 0 || ch <= 0 {
		return
	}

	memDC := v.ensureBackbuffer(hdc, cw, ch)
	barH := v.layoutToolbar(cw)

	v.mu.Lock()
	hostname := v.hostname
	rtt := v.rttMs
	activeMon := v.activeMonitor
	statusMsg := ""
	if v.statusMsg != "" && time.Now().Before(v.statusUntil) {
		statusMsg = v.statusMsg
	}
	v.mu.Unlock()

	procSetBkMode.Call(memDC, transparentBk)
	oldFont, _, _ := procSelectObject.Call(memDC, v.fontBold)

	// 1. Toolbar (only painted when visible)
	if !v.toolbarHidden && barH > 0 {
		fillSolidRect(memDC, rect{0, 0, cw, barH}, rgb(255, 255, 255))
		drawLine(memDC, 0, barH-1, cw, barH-1, rgb(228, 228, 231))

		pad := v.scale(14)
		dotSz := v.scale(9)
		dotCol := rgb(22, 163, 74)
		if rtt >= 120 {
			dotCol = rgb(220, 38, 38)
		} else if rtt >= 50 {
			dotCol = rgb(217, 119, 6)
		}
		drawStatusDot(memDC, pad, (barH-dotSz)/2, dotSz, dotCol)

		procSetTextColor.Call(memDC, rgb(24, 24, 27))
		leftLimit := v.toolbarBtns[0].rc.Left - v.scale(12)
		hostRC := rect{pad + dotSz + v.scale(9), 0, leftLimit, barH}
		drawText(memDC, hostname, &hostRC, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)

		if statusMsg != "" {
			statsStart := pad + dotSz + v.scale(9) + int32(len(hostname))*v.scale(9) + v.scale(14)
			if statsStart < leftLimit-v.scale(20) {
				procSelectObject.Call(memDC, v.fontBody)
				procSetTextColor.Call(memDC, rgb(113, 113, 122))
				statsRC := rect{statsStart, 0, leftLimit, barH}
				drawText(memDC, statusMsg, &statsRC, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)
			}
		}

		// Subtle vertical section dividers
		for _, dx := range v.toolbarDividers {
			drawLine(memDC, dx, v.scale(11), dx, barH-v.scale(11), rgb(228, 228, 231))
		}

		// Toolbar buttons
		procSelectObject.Call(memDC, v.fontBtn)
		for _, b := range v.toolbarBtns {
			hovered := v.hoverBtn == b.id
			active := (b.id == vbtnMonAll && activeMon == -1) ||
				(b.id >= vbtnMonBase && activeMon == b.id-vbtnMonBase)

			bg := rgb(255, 255, 255)
			border := rgb(212, 212, 216)
			fg := rgb(24, 24, 27)

			switch {
			case b.id == vbtnDisc:
				border = rgb(254, 202, 202)
				fg = rgb(185, 28, 28)
				if hovered {
					bg = rgb(254, 242, 242)
				}
			case b.id == vbtnHideBar:
				fg = rgb(82, 82, 91)
				if hovered {
					bg = rgb(244, 244, 245)
					fg = rgb(24, 24, 27)
				}
			case active:
				bg = rgb(24, 24, 27)
				border = rgb(24, 24, 27)
				fg = rgb(255, 255, 255)
			case hovered:
				bg = rgb(244, 244, 245)
			}
			drawFlatButton(memDC, b.rc, v.scale(5), bg, border, fg, b.label)
		}
	}

	// 2. Viewport area (dark neutral #141518).
	fillSolidRect(memDC, rect{0, barH, cw, ch}, rgb(20, 21, 24))

	stageW := cw
	stageH := ch - barH
	v.imgRect = rect{}

	v.fb.WithReadLock(func(srcW, srcH int, bgra []byte) {
		if srcW <= 0 || srcH <= 0 || len(bgra) < srcW*srcH*4 || stageW <= 0 || stageH <= 0 {
			procSelectObject.Call(memDC, v.fontBody)
			procSetTextColor.Call(memDC, rgb(161, 161, 170))
			msgRC := rect{0, barH, cw, ch}
			drawText(memDC, "Cargando...", &msgRC, dtCenter|dtSingleLn|dtVCenter)
			return
		}

		dstW := stageW
		dstH := int32(int64(stageW) * int64(srcH) / int64(srcW))
		if dstH > stageH {
			dstH = stageH
			dstW = int32(int64(stageH) * int64(srcW) / int64(srcH))
		}
		dstX := (stageW - dstW) / 2
		dstY := barH + (stageH-dstH)/2
		v.imgRect = rect{dstX, dstY, dstX + dstW, dstY + dstH}

		var bi bitmapInfo
		bi.Header.Size = uint32(unsafe.Sizeof(bi.Header))
		bi.Header.Width = int32(srcW)
		bi.Header.Height = -int32(srcH) // top-down DIB
		bi.Header.Planes = 1
		bi.Header.BitCount = 32
		bi.Header.Compression = biRGB

		// Pixel-exact 1:1 blit when unscaled, crisp HALFTONE when scaled.
		if int(dstW) == srcW && int(dstH) == srcH {
			procSetStretchBltMode.Call(memDC, colorOnColorMode)
		} else {
			procSetStretchBltMode.Call(memDC, halftoneMode)
			procSetBrushOrgEx.Call(memDC, 0, 0, 0)
		}
		procStretchDIBits.Call(
			memDC,
			uintptr(dstX), uintptr(dstY), uintptr(dstW), uintptr(dstH),
			0, 0, uintptr(srcW), uintptr(srcH),
			uintptr(unsafe.Pointer(&bgra[0])),
			uintptr(unsafe.Pointer(&bi)),
			dibRGBColors,
			srccopy,
		)
	})

	// 2.5 Draw decoupled mouse cursor overlay smoothly on top of remote desktop image.
	v.cursorMu.Lock()
	curVis := v.cursorVisible
	curX := v.cursorX
	curY := v.cursorY
	curHotX := v.cursorHotX
	curHotY := v.cursorHotY
	hCurIcon := v.cursorHIcon
	v.cursorMu.Unlock()

	if curVis && v.imgRect.Right > v.imgRect.Left {
		imgW := v.imgRect.Right - v.imgRect.Left
		imgH := v.imgRect.Bottom - v.imgRect.Top
		cx := v.imgRect.Left + int32(int64(curX)*int64(imgW)/65535) - int32(curHotX)
		cy := v.imgRect.Top + int32(int64(curY)*int64(imgH)/65535) - int32(curHotY)
		if hCurIcon != 0 {
			procDrawIconEx.Call(memDC, uintptr(cx), uintptr(cy), hCurIcon, 0, 0, 0, 0, diNormal)
		} else {
			hArrow, _, _ := procLoadCursorW.Call(0, idcArrow)
			if hArrow != 0 {
				procDrawIconEx.Call(memDC, uintptr(cx), uintptr(cy), hArrow, 0, 0, 0, 0, diNormal)
			}
		}
	}

	// 3. When toolbar is hidden, draw the compact top-center floating control capsule on top of the viewport.
	if v.toolbarHidden && v.pillBox.Right > v.pillBox.Left {
		// Slightly raise top edge so only the bottom corners are rounded like a crisp pull-tab.
		capRC := rect{v.pillBox.Left, v.pillBox.Top - v.scale(6), v.pillBox.Right, v.pillBox.Bottom}
		drawRoundedCard(memDC, capRC, v.scale(8), rgb(24, 24, 27), rgb(63, 63, 70))

		procSelectObject.Call(memDC, v.fontBtn)
		for _, b := range v.toolbarBtns {
			hovered := v.hoverBtn == b.id
			bg := rgb(24, 24, 27)
			border := rgb(24, 24, 27)
			fg := rgb(244, 244, 245)

			switch {
			case b.id == vbtnDisc:
				fg = rgb(248, 113, 113)
				if hovered {
					bg = rgb(127, 29, 29)
					border = rgb(153, 27, 27)
					fg = rgb(255, 255, 255)
				}
			case hovered:
				bg = rgb(63, 63, 70)
				border = rgb(82, 82, 91)
			}
			drawFlatButton(memDC, b.rc, v.scale(5), bg, border, fg, b.label)
		}
	}

	procSelectObject.Call(memDC, oldFont)
	procBitBlt.Call(hdc, 0, 0, uintptr(cw), uintptr(ch), memDC, 0, 0, srccopy)
}

func fillSolidRect(hdc uintptr, rc rect, color uintptr) {
	br, _, _ := procCreateSolidBrush.Call(color)
	procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rc)), br)
	procDeleteObject.Call(br)
}

func drawLine(hdc uintptr, x1, y1, x2, y2 int32, color uintptr) {
	pen, _, _ := procCreatePen.Call(psSolid, 1, color)
	oldPen, _, _ := procSelectObject.Call(hdc, pen)
	procMoveToEx.Call(hdc, uintptr(x1), uintptr(y1), 0)
	procLineTo.Call(hdc, uintptr(x2), uintptr(y2))
	procSelectObject.Call(hdc, oldPen)
	procDeleteObject.Call(pen)
}

func drawFlatButton(hdc uintptr, rc rect, radius int32, bg, border, fg uintptr, label string) {
	drawRoundedCard(hdc, rc, radius, bg, border)
	procSetTextColor.Call(hdc, fg)
	r := rc
	drawText(hdc, label, &r, dtCenter|dtSingleLn|dtVCenter)
}
