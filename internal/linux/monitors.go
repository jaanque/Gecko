//go:build linux

package linux

import (
	"encoding/binary"
	"sort"

	"remote-control/internal/clientui"
)

// EnumMonitors opens a short-lived X11 connection and returns all active monitors
// with Monitor 1 = Primary (Index 0).
func EnumMonitors() []clientui.MonitorBounds {
	xc, err := dialX11()
	if err != nil {
		return []clientui.MonitorBounds{{Index: 0, Width: 1280, Height: 720, Primary: true}}
	}
	defer xc.Close()
	return enumMonitorsOnConn(xc)
}

func enumMonitorsOnConn(xc *x11Conn) []clientui.MonitorBounds {
	fallback := []clientui.MonitorBounds{{
		Index:   0,
		X:       0,
		Y:       0,
		Width:   int(xc.rootW),
		Height:  int(xc.rootH),
		Primary: true,
	}}

	present, rrMajor, _, _, err := xc.QueryExtension("RANDR")
	if err != nil || !present {
		return fallback
	}

	// RRQueryVersion (minor 0): ask for 1.5
	var verReq [12]byte
	verReq[0] = rrMajor
	verReq[1] = 0
	binary.LittleEndian.PutUint16(verReq[2:4], 3)
	binary.LittleEndian.PutUint32(verReq[4:8], 1)
	binary.LittleEndian.PutUint32(verReq[8:12], 5)
	if _, err := xc.roundTrip(verReq[:]); err != nil {
		return fallback
	}

	// RRGetOutputPrimary (minor 31)
	var primaryOut uint32
	var primReq [8]byte
	primReq[0] = rrMajor
	primReq[1] = 31
	binary.LittleEndian.PutUint16(primReq[2:4], 2)
	binary.LittleEndian.PutUint32(primReq[4:8], xc.root)
	if rep, err := xc.roundTrip(primReq[:]); err == nil && len(rep) >= 12 {
		primaryOut = binary.LittleEndian.Uint32(rep[8:12])
	}

	// RRGetScreenResourcesCurrent (minor 25); fallback to RRGetScreenResources (minor 8).
	var resReq [8]byte
	resReq[0] = rrMajor
	resReq[1] = 25
	binary.LittleEndian.PutUint16(resReq[2:4], 2)
	binary.LittleEndian.PutUint32(resReq[4:8], xc.root)
	rep, err := xc.roundTrip(resReq[:])
	if err != nil {
		resReq[1] = 8
		rep, err = xc.roundTrip(resReq[:])
		if err != nil || len(rep) < 32 {
			return fallback
		}
	}

	cfgTS := binary.LittleEndian.Uint32(rep[12:16])
	numCrtcs := int(binary.LittleEndian.Uint16(rep[16:18]))
	if numCrtcs == 0 || 32+numCrtcs*4 > len(rep) {
		return fallback
	}

	var mons []clientui.MonitorBounds
	for i := 0; i < numCrtcs; i++ {
		crtcID := binary.LittleEndian.Uint32(rep[32+i*4 : 36+i*4])
		var ciReq [12]byte
		ciReq[0] = rrMajor
		ciReq[1] = 20 // RRGetCrtcInfo
		binary.LittleEndian.PutUint16(ciReq[2:4], 3)
		binary.LittleEndian.PutUint32(ciReq[4:8], crtcID)
		binary.LittleEndian.PutUint32(ciReq[8:12], cfgTS)

		ciRep, err := xc.roundTrip(ciReq[:])
		if err != nil || len(ciRep) < 32 || ciRep[1] != 0 {
			continue
		}
		x := int(int16(binary.LittleEndian.Uint16(ciRep[12:14])))
		y := int(int16(binary.LittleEndian.Uint16(ciRep[14:16])))
		w := int(binary.LittleEndian.Uint16(ciRep[16:18]))
		h := int(binary.LittleEndian.Uint16(ciRep[18:20]))
		mode := binary.LittleEndian.Uint32(ciRep[20:24])
		numOuts := int(binary.LittleEndian.Uint16(ciRep[28:30]))
		if mode == 0 || w <= 0 || h <= 0 {
			continue
		}

		isPrim := false
		for oi := 0; oi < numOuts && 32+(oi+1)*4 <= len(ciRep); oi++ {
			outID := binary.LittleEndian.Uint32(ciRep[32+oi*4 : 36+oi*4])
			if primaryOut != 0 && outID == primaryOut {
				isPrim = true
			}
		}

		mons = append(mons, clientui.MonitorBounds{
			X:       x,
			Y:       y,
			Width:   w,
			Height:  h,
			Primary: isPrim,
		})
	}

	if len(mons) == 0 {
		return fallback
	}

	// Ensure Primary is Monitor 1 (index 0), then left-to-right, top-to-bottom.
	sort.Slice(mons, func(i, j int) bool {
		if mons[i].Primary != mons[j].Primary {
			return mons[i].Primary
		}
		if mons[i].X != mons[j].X {
			return mons[i].X < mons[j].X
		}
		return mons[i].Y < mons[j].Y
	})
	mons[0].Primary = true
	for i := range mons {
		mons[i].Index = i
	}
	return mons
}

func resolveMonitorBoundsOnConn(xc *x11Conn, monitorIndex int) clientui.MonitorBounds {
	if monitorIndex < 0 {
		return clientui.MonitorBounds{
			Index:  -1,
			X:      0,
			Y:      0,
			Width:  int(xc.rootW),
			Height: int(xc.rootH),
		}
	}
	mons := enumMonitorsOnConn(xc)
	if monitorIndex < len(mons) {
		return mons[monitorIndex]
	}
	return mons[0]
}
