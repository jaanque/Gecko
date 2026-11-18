//go:build windows

package main

import (
	"fmt"
	"unsafe"
)

func (a *AdminWindow) paint(hwnd uintptr) {
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

	memDC, _, _ := procCreateCompatibleDC.Call(hdc)
	memBmp, _, _ := procCreateCompatibleBitmap.Call(hdc, uintptr(cw), uintptr(ch))
	oldBmp, _, _ := procSelectObject.Call(memDC, memBmp)
	defer func() {
		procSelectObject.Call(memDC, oldBmp)
		procDeleteObject.Call(memBmp)
		procDeleteDC.Call(memDC)
	}()

	a.hits = a.hits[:0]
	procSetBkMode.Call(memDC, transparentBk)

	a.mu.Lock()
	clients := make([]AdminClientRow, len(a.clients))
	copy(clients, a.clients)
	pendingHost := a.pendingHost
	noticeText := a.noticeText
	noticeIsErr := a.noticeIsError
	relayOK := a.relayConnected
	relayDetail := a.relayStatus
	hasProfile := a.hasProfile
	profile := a.profile
	a.mu.Unlock()

	fillSolidRect(memDC, cr, rgb(246, 247, 249))

	// 1. Fixed Top Navigation Bar
	barH := a.scale(54)
	pad := a.scale(20)
	fillSolidRect(memDC, rect{0, 0, cw, barH}, rgb(255, 255, 255))
	drawLine(memDC, 0, barH-1, cw, barH-1, rgb(228, 228, 231))

	oldFont, _, _ := procSelectObject.Call(memDC, a.fontBrand)
	procSetTextColor.Call(memDC, rgb(24, 24, 27))
	brandRC := rect{pad, 0, pad + a.scale(132), barH}
	drawText(memDC, "Control Remoto", &brandRC, dtLeft|dtSingleLn|dtVCenter)

	divX := brandRC.Right + a.scale(8)
	drawLine(memDC, divX, a.scale(15), divX, barH-a.scale(15), rgb(228, 228, 231))

	tabY := a.scale(11)
	tabH := barH - a.scale(22)
	tx := divX + a.scale(12)

	tab1RC := rect{tx, tabY, tx + a.scale(108), tabY + tabH}
	tab1Idx := len(a.hits)
	a.hits = append(a.hits, adminHitRegion{kind: "tab_devices", rc: tab1RC})
	tx = tab1RC.Right + a.scale(6)

	tab2RC := rect{tx, tabY, tx + a.scale(108), tabY + tabH}
	tab2Idx := len(a.hits)
	a.hits = append(a.hits, adminHitRegion{kind: "tab_builder", rc: tab2RC})
	tx = tab2RC.Right + a.scale(6)

	tab3RC := rect{tx, tabY, tx + a.scale(86), tabY + tabH}
	tab3Idx := len(a.hits)
	a.hits = append(a.hits, adminHitRegion{kind: "tab_server", rc: tab3RC})

	procSelectObject.Call(memDC, a.fontBtn)
	a.drawTopMenuTab(memDC, tab1RC, fmt.Sprintf("Equipos (%d)", len(clients)), a.activeTab == adminTabDevices, a.hoverIdx == tab1Idx)
	a.drawTopMenuTab(memDC, tab2RC, "Crear cliente", a.activeTab == adminTabBuilder, a.hoverIdx == tab2Idx)
	a.drawTopMenuTab(memDC, tab3RC, "Servidor", a.activeTab == adminTabServer, a.hoverIdx == tab3Idx)

	pillW := a.scale(116)
	pillH := a.scale(28)
	if cw-pad-pillW > tab3RC.Right+a.scale(12) {
		pillRC := rect{cw - pad - pillW, (barH - pillH) / 2, cw - pad, (barH + pillH) / 2}
		pillIdx := len(a.hits)
		a.hits = append(a.hits, adminHitRegion{kind: "tab_server", rc: pillRC})

		pillBg := rgb(246, 247, 249)
		if a.hoverIdx == pillIdx {
			pillBg = rgb(238, 240, 243)
		}
		drawRoundedCard(memDC, pillRC, a.scale(6), pillBg, rgb(228, 228, 231))
		dotCol := rgb(217, 119, 6)
		statusTxt := "Conectando..."
		if !hasProfile {
			dotCol = rgb(161, 161, 170)
			statusTxt = "Sin servidor"
		} else if relayOK {
			dotCol = rgb(22, 163, 74)
			statusTxt = "En línea"
		}
		drawStatusDot(memDC, pillRC.Left+a.scale(10), pillRC.Top+(pillH-a.scale(8))/2, a.scale(8), dotCol)
		procSelectObject.Call(memDC, a.fontSmall)
		procSetTextColor.Call(memDC, rgb(63, 63, 70))
		pTxt := rect{pillRC.Left + a.scale(24), pillRC.Top, pillRC.Right - a.scale(8), pillRC.Bottom}
		drawText(memDC, statusTxt, &pTxt, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)
	}

	// 2. Optional banner (pending request or notice)
	curY := barH + a.scale(16)
	if pendingHost != "" {
		bH := a.scale(44)
		bRC := rect{pad, curY, cw - pad, curY + bH}
		drawRoundedCard(memDC, bRC, a.scale(7), rgb(255, 255, 255), rgb(212, 212, 216))

		cancelW := a.scale(84)
		cancelH := a.scale(28)
		cancelRC := rect{bRC.Right - a.scale(10) - cancelW, curY + (bH-cancelH)/2, bRC.Right - a.scale(10), curY + (bH+cancelH)/2}
		cancelIdx := len(a.hits)
		a.hits = append(a.hits, adminHitRegion{kind: "cancel_pending", rc: cancelRC})

		drawStatusDot(memDC, bRC.Left+a.scale(14), curY+(bH-a.scale(9))/2, a.scale(9), rgb(217, 119, 6))
		procSelectObject.Call(memDC, a.fontBody)
		procSetTextColor.Call(memDC, rgb(24, 24, 27))
		msgRC := rect{bRC.Left + a.scale(32), curY, cancelRC.Left - a.scale(10), curY + bH}
		drawText(memDC, "Esperando confirmación en "+pendingHost+"...", &msgRC, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)

		cBg := rgb(255, 255, 255)
		if a.hoverIdx == cancelIdx {
			cBg = rgb(244, 244, 245)
		}
		procSelectObject.Call(memDC, a.fontBtn)
		drawFlatButton(memDC, cancelRC, a.scale(5), cBg, rgb(212, 212, 216), rgb(24, 24, 27), "Cancelar")
		curY += a.scale(56)
	} else if noticeText != "" {
		bH := a.scale(42)
		bRC := rect{pad, curY, cw - pad, curY + bH}
		bg := rgb(255, 255, 255)
		border := rgb(212, 212, 216)
		fg := rgb(24, 24, 27)
		if noticeIsErr {
			bg = rgb(254, 242, 242)
			border = rgb(254, 202, 202)
			fg = rgb(185, 28, 28)
		}
		drawRoundedCard(memDC, bRC, a.scale(7), bg, border)

		closeW := a.scale(30)
		closeRC := rect{bRC.Right - a.scale(6) - closeW, curY + a.scale(6), bRC.Right - a.scale(6), curY + bH - a.scale(6)}
		a.hits = append(a.hits, adminHitRegion{kind: "close_notice", rc: closeRC})

		procSelectObject.Call(memDC, a.fontBody)
		procSetTextColor.Call(memDC, fg)
		msgRC := rect{bRC.Left + a.scale(14), curY, closeRC.Left - a.scale(8), curY + bH}
		drawText(memDC, noticeText, &msgRC, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)

		closeTxtRC := closeRC
		drawText(memDC, "×", &closeTxtRC, dtCenter|dtSingleLn|dtVCenter)
		curY += a.scale(56)
	}

	// 3. Active tab content
	switch a.activeTab {
	case adminTabDevices:
		a.paintDevicesTab(memDC, cw, ch, pad, curY, clients, hasProfile, relayOK, relayDetail)
	case adminTabBuilder:
		a.paintBuilderTab(memDC, cw, ch, pad, curY, hasProfile)
	case adminTabServer:
		a.paintServerTab(memDC, cw, ch, hasProfile, relayOK, relayDetail, profile)
	}

	procSelectObject.Call(memDC, oldFont)
	procBitBlt.Call(hdc, 0, 0, uintptr(cw), uintptr(ch), memDC, 0, 0, srccopy)
}

func (a *AdminWindow) drawTopMenuTab(hdc uintptr, rc rect, label string, active, hovered bool) {
	if active {
		drawFlatButton(hdc, rc, a.scale(6), rgb(24, 24, 27), rgb(24, 24, 27), rgb(255, 255, 255), label)
		return
	}
	bg := rgb(255, 255, 255)
	if hovered {
		bg = rgb(244, 244, 245)
	}
	drawFlatButton(hdc, rc, a.scale(6), bg, bg, rgb(63, 63, 70), label)
}

func drawRoundedCard(hdc uintptr, rc rect, radius int32, bg, border uintptr) {
	br, _, _ := procCreateSolidBrush.Call(bg)
	pen, _, _ := procCreatePen.Call(psSolid, 1, border)
	oldBr, _, _ := procSelectObject.Call(hdc, br)
	oldPen, _, _ := procSelectObject.Call(hdc, pen)
	procRoundRect.Call(
		hdc,
		uintptr(rc.Left), uintptr(rc.Top), uintptr(rc.Right), uintptr(rc.Bottom),
		uintptr(radius), uintptr(radius),
	)
	procSelectObject.Call(hdc, oldBr)
	procSelectObject.Call(hdc, oldPen)
	procDeleteObject.Call(br)
	procDeleteObject.Call(pen)
}
