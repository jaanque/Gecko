//go:build windows

package win

import (
	"fmt"

	"remote-control/internal/protocol"
)

type viewerToolbarBtn struct {
	id    int
	label string
	rc    rect
}

const (
	vbtnQualLow  = 1
	vbtnQualMed  = 2
	vbtnQualHi   = 3
	vbtnFull     = 4
	vbtnDisc     = 5
	vbtnHideBar  = 6
	vbtnShowBar  = 7
	vbtnSendFile = 8
	vbtnCAD      = 9
	vbtnMonAll   = 100
	vbtnMonBase  = 200 // 200 + monitorIndex (0, 1, 2, ...)

	qualLowVal = 55
	qualMedVal = 78
	qualHiVal  = 92
)

func (v *remoteViewer) layoutToolbar(width int32) int32 {
	v.toolbarDividers = v.toolbarDividers[:0]
	v.pillBox = rect{}

	if v.toolbarHidden {
		pillH := v.scale(28)
		padX := v.scale(4)
		padY := v.scale(3)
		gap := v.scale(4)
		btnH := pillH - padY*2

		showW := v.scale(78)
		discW := v.scale(30)
		fullW := v.scale(58)

		totalW := padX*2 + showW + gap + discW
		if v.fullscreen {
			totalW += gap + fullW
		}
		px := (width - totalW) / 2
		v.pillBox = rect{px, 0, px + totalW, pillH}

		x := px + padX
		btns := make([]viewerToolbarBtn, 0, 3)
		btns = append(btns, viewerToolbarBtn{
			id:    vbtnShowBar,
			label: "▼ Menú",
			rc:    rect{x, padY, x + showW, padY + btnH},
		})
		x += showW + gap

		if v.fullscreen {
			btns = append(btns, viewerToolbarBtn{
				id:    vbtnFull,
				label: "Salir",
				rc:    rect{x, padY, x + fullW, padY + btnH},
			})
			x += fullW + gap
		}

		btns = append(btns, viewerToolbarBtn{
			id:    vbtnDisc,
			label: "×",
			rc:    rect{x, padY, x + discW, padY + btnH},
		})

		v.toolbarBtns = btns
		return 0
	}

	barH := v.scale(44)
	pad := v.scale(12)
	btnH := v.scale(28)
	btnY := (barH - btnH) / 2
	gap := v.scale(6)
	smallGap := v.scale(4)

	hideW := v.scale(30)
	discW := v.scale(94)
	fullW := v.scale(116)
	cadW := v.scale(52)
	fileW := v.scale(64)
	qHiW := v.scale(44)
	qMedW := v.scale(66)
	qLowW := v.scale(44)

	x := width - pad

	rcHide := rect{x - hideW, btnY, x, btnY + btnH}
	x = rcHide.Left - gap

	rcDisc := rect{x - discW, btnY, x, btnY + btnH}
	x = rcDisc.Left - gap

	fullLabel := "Pantalla comp."
	if v.fullscreen {
		fullLabel = "Salir pantalla"
	}
	rcFull := rect{x - fullW, btnY, x, btnY + btnH}
	x = rcFull.Left - gap

	rcCAD := rect{x - cadW, btnY, x, btnY + btnH}
	x = rcCAD.Left - smallGap

	rcFile := rect{x - fileW, btnY, x, btnY + btnH}
	x = rcFile.Left - v.scale(8)
	v.toolbarDividers = append(v.toolbarDividers, x)
	x -= v.scale(8)

	rcHi := rect{x - qHiW, btnY, x, btnY + btnH}
	x = rcHi.Left - smallGap
	rcMed := rect{x - qMedW, btnY, x, btnY + btnH}
	x = rcMed.Left - smallGap
	rcLow := rect{x - qLowW, btnY, x, btnY + btnH}
	x = rcLow.Left

	btns := make([]viewerToolbarBtn, 0, 16)

	v.mu.Lock()
	monCount := v.monitorCount
	v.mu.Unlock()
	if monCount > 1 {
		if monCount > 6 {
			monCount = 6
		}
		x -= v.scale(8)
		v.toolbarDividers = append(v.toolbarDividers, x)
		x -= v.scale(8)

		allW := v.scale(54)
		rcAll := rect{x - allW, btnY, x, btnY + btnH}
		x = rcAll.Left - smallGap

		monBtns := make([]viewerToolbarBtn, 0, monCount+1)
		monW := v.scale(36)
		for i := monCount - 1; i >= 0; i-- {
			rcM := rect{x - monW, btnY, x, btnY + btnH}
			monBtns = append(monBtns, viewerToolbarBtn{
				id:    vbtnMonBase + i,
				label: fmt.Sprintf("M%d", i+1),
				rc:    rcM,
			})
			x = rcM.Left - smallGap
		}
		for i := len(monBtns) - 1; i >= 0; i-- {
			btns = append(btns, monBtns[i])
		}
		btns = append(btns, viewerToolbarBtn{id: vbtnMonAll, label: "Todos", rc: rcAll})
	}

	btns = append(btns,
		viewerToolbarBtn{id: vbtnQualLow, label: "Baja", rc: rcLow},
		viewerToolbarBtn{id: vbtnQualMed, label: "Estándar", rc: rcMed},
		viewerToolbarBtn{id: vbtnQualHi, label: "Alta", rc: rcHi},
		viewerToolbarBtn{id: vbtnSendFile, label: "Archivo", rc: rcFile},
		viewerToolbarBtn{id: vbtnCAD, label: "CAD", rc: rcCAD},
		viewerToolbarBtn{id: vbtnFull, label: fullLabel, rc: rcFull},
		viewerToolbarBtn{id: vbtnDisc, label: "Desconectar", rc: rcDisc},
		viewerToolbarBtn{id: vbtnHideBar, label: "▲", rc: rcHide},
	)
	v.toolbarBtns = btns
	return barH
}

func (v *remoteViewer) hitTestToolbar(x, y int32) int {
	for _, b := range v.toolbarBtns {
		if x >= b.rc.Left && x <= b.rc.Right && y >= b.rc.Top && y <= b.rc.Bottom {
			return b.id
		}
	}
	return 0
}

func (v *remoteViewer) isInCollapsedPill(x, y int32) bool {
	if !v.toolbarHidden {
		return false
	}
	p := v.pillBox
	return p.Right > p.Left && x >= p.Left && x <= p.Right && y >= p.Top && y <= p.Bottom
}

func (v *remoteViewer) handleToolbarClick(hwnd uintptr, btnID int) {
	switch {
	case btnID == vbtnQualLow:
		v.setQuality(hwnd, qualLowVal)
	case btnID == vbtnQualMed:
		v.setQuality(hwnd, qualMedVal)
	case btnID == vbtnQualHi:
		v.setQuality(hwnd, qualHiVal)
	case btnID == vbtnMonAll:
		v.selectMonitor(hwnd, -1)
	case btnID >= vbtnMonBase:
		v.selectMonitor(hwnd, btnID-vbtnMonBase)
	case btnID == vbtnFull:
		v.toggleFullscreen(hwnd)
	case btnID == vbtnSendFile:
		if v.cb.OnSendFile != nil {
			if path, ok := ShowOpenAnyFileDialog(hwnd); ok && path != "" {
				go func(p string) {
					if err := v.cb.OnSendFile(p); err != nil {
						v.SetFileStatus("", false, "Error: "+err.Error())
					}
				}(path)
			}
		}
	case btnID == vbtnCAD:
		if v.cb.OnSystemAction != nil {
			v.SetFileStatus("", true, "Enviado Ctrl+Alt+Supr")
			go v.cb.OnSystemAction(protocol.ActionCtrlAltDel)
		}
	case btnID == vbtnHideBar:
		v.toolbarHidden = true
		v.hoverBtn = 0
		procInvalidateRect.Call(hwnd, 0, 0)
	case btnID == vbtnShowBar:
		v.toolbarHidden = false
		v.hoverBtn = 0
		procInvalidateRect.Call(hwnd, 0, 0)
	case btnID == vbtnDisc:
		if v.cb.OnDisconnect != nil {
			go v.cb.OnDisconnect()
		}
		v.CloseUI()
	}
}

func (v *remoteViewer) setQuality(hwnd uintptr, q int) {
	v.mu.Lock()
	v.quality = q
	v.mu.Unlock()
	if v.cb.OnQuality != nil {
		go v.cb.OnQuality(q)
	}
	procInvalidateRect.Call(hwnd, 0, 0)
}

func (v *remoteViewer) selectMonitor(hwnd uintptr, monitorIndex int) {
	v.mu.Lock()
	if v.activeMonitor == monitorIndex {
		v.mu.Unlock()
		return
	}
	v.activeMonitor = monitorIndex
	v.mu.Unlock()

	if v.cb.OnSelectMonitor != nil {
		go v.cb.OnSelectMonitor(monitorIndex)
	}
	procInvalidateRect.Call(hwnd, 0, 0)
}
