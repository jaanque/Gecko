//go:build windows

package win

import (
	"fmt"
	"path/filepath"
	"strings"
	"unsafe"

	"remote-control/internal/config"
	"remote-control/internal/security"
)

func (a *AdminWindow) contentTopY() int32 {
	top := a.scale(54) + a.scale(16)
	a.mu.Lock()
	hasBanner := a.pendingHost != "" || a.noticeText != ""
	a.mu.Unlock()
	if hasBanner {
		top += a.scale(56)
	}
	return top
}

func (a *AdminWindow) serverCardMetrics(cw, ch int32) (cardRC rect, innerL, innerW int32, addrY, keyY int32) {
	pad := a.scale(20)
	cardRC = rect{pad, a.contentTopY(), cw - pad, ch - pad}
	innerPad := a.scale(24)
	innerL = cardRC.Left + innerPad
	innerW = (cardRC.Right - cardRC.Left) - innerPad*2
	addrY = cardRC.Top + a.scale(24)
	keyY = addrY + a.scale(68)
	return
}

func (a *AdminWindow) syncServerControls(hwnd uintptr) {
	if a.ctrlSrvAddr == 0 {
		return
	}
	var cr rect
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&cr)))
	cw := cr.Right - cr.Left
	ch := cr.Bottom - cr.Top

	if a.ctrlSearch != 0 {
		a.mu.Lock()
		hasClients := len(a.clients) > 0
		a.mu.Unlock()
		if a.activeTab == adminTabDevices && hasClients && cw > 0 {
			pad := a.scale(20)
			topY := a.contentTopY()
			procSetWindowPos.Call(a.ctrlSearch, 0, uintptr(pad), uintptr(topY), uintptr(cw-pad*2), uintptr(a.scale(28)), 0)
			procShowWindow.Call(a.ctrlSearch, swShowNormal)
		} else {
			procShowWindow.Call(a.ctrlSearch, swHide)
		}
	}

	if a.activeTab != adminTabServer {
		procShowWindow.Call(a.ctrlSrvAddr, swHide)
		procShowWindow.Call(a.ctrlSrvKey, swHide)
		return
	}
	if cw <= 0 || ch <= 0 {
		return
	}

	_, innerL, innerW, addrY, keyY := a.serverCardMetrics(cw, ch)
	lblH := a.scale(22)
	editH := a.scale(28)
	pasteW := a.scale(78)
	gap := a.scale(8)

	procSetWindowPos.Call(a.ctrlSrvAddr, 0, uintptr(innerL), uintptr(addrY+lblH), uintptr(innerW), uintptr(editH), 0)
	procSetWindowPos.Call(a.ctrlSrvKey, 0, uintptr(innerL), uintptr(keyY+lblH), uintptr(innerW-pasteW-gap), uintptr(editH), 0)
	procShowWindow.Call(a.ctrlSrvAddr, swShowNormal)
	procShowWindow.Call(a.ctrlSrvKey, swShowNormal)
}

func (a *AdminWindow) switchTab(hwnd uintptr, tab int) {
	if a.activeTab == tab {
		return
	}
	a.activeTab = tab
	a.hoverIdx = -1
	a.syncServerControls(hwnd)
	procInvalidateRect.Call(hwnd, 0, 0)
}

func (a *AdminWindow) paintDevicesTab(memDC uintptr, cw, ch, pad, topY int32, clients []AdminClientRow, hasProfile, relayOK bool, relayDetail string) {
	if len(clients) > 0 {
		topY += a.scale(38) // leave room for search bar
	}
	cardRC := rect{pad, topY, cw - pad, ch - pad}
	if cardRC.Bottom-cardRC.Top < a.scale(140) {
		cardRC.Bottom = cardRC.Top + a.scale(140)
	}
	drawRoundedCard(memDC, cardRC, a.scale(8), rgb(255, 255, 255), rgb(228, 228, 231))

	listTop := cardRC.Top + a.scale(6)
	listBottom := cardRC.Bottom - a.scale(6)
	listH := listBottom - listTop

	if len(clients) == 0 {
		a.maxScrollY = 0
		a.scrollY = 0
		midY := listTop + listH/2

		title := "No hay equipos conectados"
		ctaLabel := "Crear cliente"
		ctaKind := "tab_builder"

		if !hasProfile {
			title = "Servidor sin configurar"
			ctaLabel = "Configurar servidor"
			ctaKind = "tab_server"
		} else if !relayOK {
			title = relayDetail
			ctaLabel = "Revisar servidor"
			ctaKind = "tab_server"
		}

		procSelectObject.Call(memDC, a.fontTitle)
		procSetTextColor.Call(memDC, rgb(24, 24, 27))
		tRC := rect{cardRC.Left + a.scale(24), midY - a.scale(30), cardRC.Right - a.scale(24), midY - a.scale(4)}
		drawText(memDC, title, &tRC, dtCenter|dtSingleLn|dtVCenter|dtEndEllip)

		ctaW := a.scale(168)
		ctaH := a.scale(34)
		ctaX := cardRC.Left + (cardRC.Right-cardRC.Left-ctaW)/2
		ctaRC := rect{ctaX, midY + a.scale(8), ctaX + ctaW, midY + a.scale(8) + ctaH}
		ctaIdx := len(a.hits)
		a.hits = append(a.hits, adminHitRegion{kind: ctaKind, rc: ctaRC})
		ctaBg := rgb(24, 24, 27)
		if a.hoverIdx == ctaIdx {
			ctaBg = rgb(39, 39, 42)
		}
		procSelectObject.Call(memDC, a.fontBtn)
		drawFlatButton(memDC, ctaRC, a.scale(6), ctaBg, ctaBg, rgb(255, 255, 255), ctaLabel)
		return
	}

	filtered := filterAdminClients(clients, a.searchQuery)
	if len(filtered) == 0 {
		a.maxScrollY = 0
		a.scrollY = 0
		procSelectObject.Call(memDC, a.fontBody)
		procSetTextColor.Call(memDC, rgb(113, 113, 122))
		emptyRC := rect{cardRC.Left + a.scale(20), listTop, cardRC.Right - a.scale(20), listBottom}
		drawText(memDC, "Ningún equipo coincide con la búsqueda", &emptyRC, dtCenter|dtSingleLn|dtVCenter)
		return
	}

	rowH := a.scale(62)
	totalH := int32(len(filtered)) * rowH
	if totalH > listH {
		a.maxScrollY = totalH - listH
	} else {
		a.maxScrollY = 0
	}
	if a.scrollY > a.maxScrollY {
		a.scrollY = a.maxScrollY
	}
	if a.scrollY < 0 {
		a.scrollY = 0
	}

	for i, c := range filtered {
		ry := listTop + int32(i)*rowH - a.scrollY
		if ry+rowH < listTop || ry > listBottom {
			continue
		}

		visTop := ry
		if visTop < listTop {
			visTop = listTop
		}
		visBot := ry + rowH
		if visBot > listBottom {
			visBot = listBottom
		}
		rowRC := rect{cardRC.Left + 1, visTop, cardRC.Right - 1, visBot}

		btnW := a.scale(86)
		revW := a.scale(72)
		btnH := a.scale(30)
		btnGap := a.scale(8)
		btnTop := ry + (rowH-btnH)/2

		btnRC := rect{cardRC.Right - a.scale(16) - btnW, btnTop, cardRC.Right - a.scale(16), btnTop + btnH}
		revRC := rect{btnRC.Left - btnGap - revW, btnTop, btnRC.Left - btnGap, btnTop + btnH}

		revIdx := -1
		if visBot-visTop >= a.scale(34) && c.CredentialID != "" {
			revIdx = len(a.hits)
			a.hits = append(a.hits, adminHitRegion{
				kind:     "revoke",
				clientID: c.ID,
				credID:   c.CredentialID,
				hostname: c.Hostname,
				rc:       revRC,
			})
		}

		rowIdx := -1
		if c.Status == "online" {
			rowIdx = len(a.hits)
			a.hits = append(a.hits, adminHitRegion{kind: "row", clientID: c.ID, rc: rowRC})
		}

		if rowIdx >= 0 && a.hoverIdx == rowIdx {
			fillSolidRect(memDC, rowRC, rgb(250, 250, 250))
		}
		if i < len(filtered)-1 && ry+rowH <= listBottom {
			drawLine(memDC, cardRC.Left+a.scale(16), ry+rowH-1, cardRC.Right-a.scale(16), ry+rowH-1, rgb(244, 244, 245))
		}
		if visBot-visTop < a.scale(34) {
			continue
		}

		dotColor := rgb(22, 163, 74)
		statusLabel := "Disponible"
		switch c.Status {
		case "pending":
			dotColor = rgb(217, 119, 6)
			statusLabel = "Esperando..."
		case "active":
			dotColor = rgb(24, 24, 27)
			statusLabel = "En sesión"
		case "offline":
			dotColor = rgb(161, 161, 170)
			statusLabel = "Desconectado"
		}

		dotSz := a.scale(9)
		drawStatusDot(memDC, cardRC.Left+a.scale(18), ry+(rowH-dotSz)/2, dotSz, dotColor)

		statusRight := btnRC.Left - a.scale(12)
		if revIdx >= 0 {
			statusRight = revRC.Left - a.scale(12)
		}
		statusW := a.scale(88)
		statusRC := rect{statusRight - statusW, ry, statusRight, ry + rowH}

		textLeft := cardRC.Left + a.scale(38)
		textRight := statusRC.Left - a.scale(10)

		procSelectObject.Call(memDC, a.fontRow)
		procSetTextColor.Call(memDC, rgb(24, 24, 27))
		nameRC := rect{textLeft, ry + a.scale(10), textRight, ry + a.scale(32)}
		drawText(memDC, c.Hostname, &nameRC, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)

		if c.Username != "" {
			procSelectObject.Call(memDC, a.fontBody)
			procSetTextColor.Call(memDC, rgb(113, 113, 122))
			userRC := rect{textLeft, ry + a.scale(33), textRight, ry + a.scale(52)}
			drawText(memDC, c.Username, &userRC, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)
		}

		procSelectObject.Call(memDC, a.fontBody)
		procSetTextColor.Call(memDC, rgb(82, 82, 91))
		drawText(memDC, statusLabel, &statusRC, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)

		procSelectObject.Call(memDC, a.fontBtn)
		if revIdx >= 0 {
			rBg := rgb(255, 255, 255)
			if a.hoverIdx == revIdx {
				rBg = rgb(254, 242, 242)
			}
			drawFlatButton(memDC, revRC, a.scale(5), rBg, rgb(254, 202, 202), rgb(185, 28, 28), "Revocar")
		}
		if c.Status == "online" {
			bBg := rgb(255, 255, 255)
			if rowIdx >= 0 && a.hoverIdx == rowIdx {
				bBg = rgb(244, 244, 245)
			}
			drawFlatButton(memDC, btnRC, a.scale(5), bBg, rgb(212, 212, 216), rgb(24, 24, 27), "Conectar")
		} else {
			drawFlatButton(memDC, btnRC, a.scale(5), rgb(250, 250, 250), rgb(228, 228, 231), rgb(161, 161, 170), "Conectar")
		}
	}
}

func filterAdminClients(clients []AdminClientRow, query string) []AdminClientRow {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return clients
	}
	out := make([]AdminClientRow, 0, len(clients))
	for _, c := range clients {
		if strings.Contains(strings.ToLower(c.Hostname), q) ||
			strings.Contains(strings.ToLower(c.Username), q) ||
			strings.Contains(strings.ToLower(c.IP), q) {
			out = append(out, c)
		}
	}
	return out
}

func (a *AdminWindow) paintBuilderTab(memDC uintptr, cw, ch, pad, topY int32, hasProfile bool) {
	cardRC := rect{pad, topY, cw - pad, ch - pad}
	drawRoundedCard(memDC, cardRC, a.scale(8), rgb(255, 255, 255), rgb(228, 228, 231))

	innerL := cardRC.Left + a.scale(24)
	rightEdge := cardRC.Right - a.scale(24)
	rowY := cardRC.Top + a.scale(28)

	boxSz := a.scale(56)
	boxRC := rect{innerL, rowY, innerL + boxSz, rowY + boxSz}
	drawRoundedCard(memDC, boxRC, a.scale(8), rgb(246, 247, 249), rgb(228, 228, 231))
	if a.logoPreview != nil {
		inner := rect{boxRC.Left + a.scale(5), boxRC.Top + a.scale(5), boxRC.Right - a.scale(5), boxRC.Bottom - a.scale(5)}
		a.logoPreview.DrawFit(memDC, inner)
	}

	pickW := a.scale(88)
	remW := a.scale(68)
	btnH := a.scale(32)
	btnY := rowY + (boxSz-btnH)/2

	pickRight := rightEdge
	if len(a.logoPNG) > 0 {
		remRC := rect{rightEdge - remW, btnY, rightEdge, btnY + btnH}
		remIdx := len(a.hits)
		a.hits = append(a.hits, adminHitRegion{kind: "remove_logo", rc: remRC})
		remBg := rgb(255, 255, 255)
		if a.hoverIdx == remIdx {
			remBg = rgb(254, 242, 242)
		}
		procSelectObject.Call(memDC, a.fontBtn)
		drawFlatButton(memDC, remRC, a.scale(5), remBg, rgb(254, 202, 202), rgb(185, 28, 28), "Quitar")
		pickRight = remRC.Left - a.scale(8)
	}

	pickRC := rect{pickRight - pickW, btnY, pickRight, btnY + btnH}
	pickIdx := len(a.hits)
	a.hits = append(a.hits, adminHitRegion{kind: "pick_logo", rc: pickRC})
	pickBg := rgb(255, 255, 255)
	if a.hoverIdx == pickIdx {
		pickBg = rgb(244, 244, 245)
	}
	procSelectObject.Call(memDC, a.fontBtn)
	drawFlatButton(memDC, pickRC, a.scale(5), pickBg, rgb(212, 212, 216), rgb(24, 24, 27), "Elegir")

	txtLeft := boxRC.Right + a.scale(14)
	txtRight := pickRC.Left - a.scale(12)
	procSelectObject.Call(memDC, a.fontTitle)
	procSetTextColor.Call(memDC, rgb(24, 24, 27))
	lt1 := rect{txtLeft, rowY + a.scale(6), txtRight, rowY + a.scale(28)}
	drawText(memDC, "Logo", &lt1, dtLeft|dtSingleLn|dtVCenter)

	procSelectObject.Call(memDC, a.fontBody)
	procSetTextColor.Call(memDC, rgb(113, 113, 122))
	logoSub := "Sin logo"
	if a.logoName != "" {
		logoSub = a.logoName
	}
	lt2 := rect{txtLeft, rowY + a.scale(28), txtRight, rowY + a.scale(50)}
	drawText(memDC, logoSub, &lt2, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)

	// Client UI language selector row ("Idioma del cliente")
	langRowY := rowY + boxSz + a.scale(22)
	drawLine(memDC, innerL, langRowY-a.scale(12), rightEdge, langRowY-a.scale(12), rgb(244, 244, 245))

	procSelectObject.Call(memDC, a.fontTitle)
	procSetTextColor.Call(memDC, rgb(24, 24, 27))
	langLblRC := rect{innerL, langRowY, innerL + a.scale(180), langRowY + a.scale(32)}
	drawText(memDC, "Idioma del cliente", &langLblRC, dtLeft|dtSingleLn|dtVCenter)

	langOpts := []struct {
		code  string
		label string
	}{
		{"ca", "Català"},
		{"es", "Español"},
		{"en", "English"},
	}
	pillW := a.scale(78)
	pillH := a.scale(30)
	pillGap := a.scale(6)
	lx := rightEdge - int32(len(langOpts))*pillW - int32(len(langOpts)-1)*pillGap
	procSelectObject.Call(memDC, a.fontBtn)
	for _, opt := range langOpts {
		prc := rect{lx, langRowY + a.scale(1), lx + pillW, langRowY + a.scale(1) + pillH}
		idx := len(a.hits)
		a.hits = append(a.hits, adminHitRegion{kind: "lang_" + opt.code, rc: prc})
		active := a.clientLang == opt.code
		if active {
			drawFlatButton(memDC, prc, a.scale(5), rgb(24, 24, 27), rgb(24, 24, 27), rgb(255, 255, 255), opt.label)
		} else {
			bg := rgb(255, 255, 255)
			if a.hoverIdx == idx {
				bg = rgb(244, 244, 245)
			}
			drawFlatButton(memDC, prc, a.scale(5), bg, rgb(212, 212, 216), rgb(24, 24, 27), opt.label)
		}
		lx += pillW + pillGap
	}

	footerH := a.scale(58)
	footerTop := cardRC.Bottom - footerH
	if footerTop < langRowY+a.scale(48) {
		footerTop = langRowY + a.scale(48)
	}
	drawLine(memDC, cardRC.Left+1, footerTop, cardRC.Right-1, footerTop, rgb(228, 228, 231))

	saveWinW := a.scale(196)
	saveH := a.scale(36)
	btnTop := footerTop + (footerH-saveH)/2

	saveWinRC := rect{rightEdge - saveWinW, btnTop, rightEdge, btnTop + saveH}
	saveWinIdx := len(a.hits)
	a.hits = append(a.hits, adminHitRegion{kind: "save_client_win", rc: saveWinRC})

	if !hasProfile {
		procSelectObject.Call(memDC, a.fontSmall)
		procSetTextColor.Call(memDC, rgb(185, 28, 28))
		fRC := rect{innerL, footerTop, saveWinRC.Left - a.scale(12), footerTop + footerH}
		drawText(memDC, "Configura el servidor primero", &fRC, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)
	}

	winBg := rgb(24, 24, 27)
	if a.hoverIdx == saveWinIdx {
		winBg = rgb(39, 39, 42)
	}
	procSelectObject.Call(memDC, a.fontBtn)
	drawFlatButton(memDC, saveWinRC, a.scale(6), winBg, winBg, rgb(255, 255, 255), "Guardar cliente")
}

func (a *AdminWindow) paintServerTab(memDC uintptr, cw, ch int32, hasProfile, relayOK bool, relayDetail string, profile security.RelayProfile) {
	cardRC, innerL, innerW, addrY, keyY := a.serverCardMetrics(cw, ch)
	drawRoundedCard(memDC, cardRC, a.scale(8), rgb(255, 255, 255), rgb(228, 228, 231))
	rightEdge := innerL + innerW

	procSelectObject.Call(memDC, a.fontBtn)
	procSetTextColor.Call(memDC, rgb(24, 24, 27))
	l1 := rect{innerL, addrY, rightEdge, addrY + a.scale(20)}
	drawText(memDC, "Dirección", &l1, dtLeft|dtSingleLn|dtVCenter)

	l2 := rect{innerL, keyY, rightEdge, keyY + a.scale(20)}
	drawText(memDC, "Clave", &l2, dtLeft|dtSingleLn|dtVCenter)

	pasteW := a.scale(78)
	pasteH := a.scale(28)
	pasteRC := rect{rightEdge - pasteW, keyY + a.scale(22), rightEdge, keyY + a.scale(22) + pasteH}
	pasteIdx := len(a.hits)
	a.hits = append(a.hits, adminHitRegion{kind: "paste_key", rc: pasteRC})
	pasteBg := rgb(255, 255, 255)
	if a.hoverIdx == pasteIdx {
		pasteBg = rgb(244, 244, 245)
	}
	drawFlatButton(memDC, pasteRC, a.scale(5), pasteBg, rgb(212, 212, 216), rgb(24, 24, 27), "Pegar")

	stTop := keyY + a.scale(66)
	stH := a.scale(40)
	stRC := rect{innerL, stTop, rightEdge, stTop + stH}
	drawRoundedCard(memDC, stRC, a.scale(6), rgb(249, 250, 251), rgb(228, 228, 231))

	dotCol := rgb(161, 161, 170)
	stLine := "Sin configurar"
	if hasProfile {
		if relayOK {
			dotCol = rgb(22, 163, 74)
			stLine = "Conectado"
		} else {
			dotCol = rgb(217, 119, 6)
			stLine = relayDetail
		}
	}
	drawStatusDot(memDC, stRC.Left+a.scale(14), stRC.Top+(stH-a.scale(9))/2, a.scale(9), dotCol)
	procSelectObject.Call(memDC, a.fontBody)
	procSetTextColor.Call(memDC, rgb(24, 24, 27))
	sRC := rect{stRC.Left + a.scale(32), stRC.Top, stRC.Right - a.scale(12), stRC.Bottom}
	drawText(memDC, stLine, &sRC, dtLeft|dtSingleLn|dtVCenter|dtEndEllip)

	footerH := a.scale(58)
	footerTop := cardRC.Bottom - footerH
	if footerTop < stRC.Bottom+a.scale(14) {
		footerTop = stRC.Bottom + a.scale(14)
	}
	drawLine(memDC, cardRC.Left+1, footerTop, cardRC.Right-1, footerTop, rgb(228, 228, 231))

	saveW := a.scale(164)
	saveH := a.scale(36)
	saveRC := rect{rightEdge - saveW, footerTop + (footerH-saveH)/2, rightEdge, footerTop + (footerH+saveH)/2}
	saveIdx := len(a.hits)
	a.hits = append(a.hits, adminHitRegion{kind: "save_server", rc: saveRC})

	saveBg := rgb(24, 24, 27)
	if a.hoverIdx == saveIdx {
		saveBg = rgb(39, 39, 42)
	}
	procSelectObject.Call(memDC, a.fontBtn)
	drawFlatButton(memDC, saveRC, a.scale(6), saveBg, saveBg, rgb(255, 255, 255), "Guardar y conectar")
}

func (a *AdminWindow) handleSaveServer(hwnd uintptr) {
	keyStr := strings.TrimSpace(getControlText(a.ctrlSrvKey))
	prof, err := security.DecodeProfileKey(keyStr)
	if err != nil {
		ShowMessageBox(hwnd, "Clave inválida", err.Error(), true)
		return
	}

	addr := strings.TrimSpace(getControlText(a.ctrlSrvAddr))
	if addr != "" {
		prof.ServerAddr = addr
	}
	if prof.ServerAddr == "" {
		ShowMessageBox(hwnd, "Dirección requerida", "Introduce host:puerto del servidor.", true)
		return
	}
	if !strings.Contains(prof.ServerAddr, ":") {
		prof.ServerAddr += ":9443"
	}

	if a.callbacks.OnSaveServer != nil {
		if err := a.callbacks.OnSaveServer(prof); err != nil {
			ShowMessageBox(hwnd, "Error", err.Error(), true)
			return
		}
	}

	a.mu.Lock()
	a.profile = prof
	a.hasProfile = true
	a.noticeText = ""
	a.noticeIsError = false
	a.mu.Unlock()

	setControlText(a.ctrlSrvAddr, prof.ServerAddr)
	a.switchTab(hwnd, adminTabDevices)
}

func (a *AdminWindow) handlePickLogo(hwnd uintptr) {
	path, ok := ShowOpenLogoDialog(hwnd)
	if !ok || path == "" {
		return
	}
	pngBytes, err := config.LoadAndNormalizeLogo(path)
	if err != nil {
		ShowMessageBox(hwnd, "Logo inválido", err.Error(), true)
		return
	}
	if a.logoPreview != nil {
		a.logoPreview.Close()
	}
	a.logoPNG = pngBytes
	a.logoName = filepath.Base(path)
	a.logoPreview = newLogoGDI(pngBytes)
	procInvalidateRect.Call(hwnd, 0, 0)
}

func (a *AdminWindow) handleSaveClient(hwnd uintptr) {
	a.mu.Lock()
	hasProf := a.hasProfile
	a.mu.Unlock()
	if !hasProf {
		a.switchTab(hwnd, adminTabServer)
		return
	}

	destPath, ok := ShowSaveExeDialog(hwnd, "ClientRemot.exe")
	if !ok || destPath == "" {
		return
	}

	if a.callbacks.OnGenerateClient != nil {
		if err := a.callbacks.OnGenerateClient(a.logoPNG, a.clientLang, destPath); err != nil {
			ShowMessageBox(hwnd, "Error", err.Error(), true)
			return
		}
	}

	a.mu.Lock()
	a.noticeText = "Guardado: " + filepath.Base(destPath)
	a.noticeIsError = false
	a.mu.Unlock()
	a.syncServerControls(hwnd)
	procInvalidateRect.Call(hwnd, 0, 0)
}
