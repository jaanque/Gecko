//go:build windows

package win

import (
	"sort"
	"sync"
	"syscall"
	"unsafe"
)

var (
	procEnumDisplayMonitors = user32GUI.NewProc("EnumDisplayMonitors")
	procGetMonitorInfoW     = user32GUI.NewProc("GetMonitorInfoW")
)

const monitorInfoPrimary = 0x00000001

// MonitorBounds describes a physical display monitor in virtual desktop coordinates.
type MonitorBounds struct {
	Index   int
	X       int
	Y       int
	Width   int
	Height  int
	Primary bool
}

var (
	enumMonMu   sync.Mutex
	enumMonList []MonitorBounds
	enumMonCB   = syscall.NewCallback(enumMonitorProc)
)

func enumMonitorProc(hMon, _ uintptr, _ uintptr, _ uintptr) uintptr {
	var mi monitorInfo
	mi.CbSize = uint32(unsafe.Sizeof(mi))
	if ok, _, _ := procGetMonitorInfoW.Call(hMon, uintptr(unsafe.Pointer(&mi))); ok != 0 {
		w := int(mi.RcMonitor.Right - mi.RcMonitor.Left)
		h := int(mi.RcMonitor.Bottom - mi.RcMonitor.Top)
		if w > 0 && h > 0 {
			enumMonList = append(enumMonList, MonitorBounds{
				X:       int(mi.RcMonitor.Left),
				Y:       int(mi.RcMonitor.Top),
				Width:   w,
				Height:  h,
				Primary: (mi.DwFlags & monitorInfoPrimary) != 0,
			})
		}
	}
	return 1
}

// VirtualDesktopBounds returns the full bounding rectangle spanning all monitors.
func VirtualDesktopBounds() MonitorBounds {
	vxRaw, _, _ := procGetSystemMetrics.Call(smXVirtualScreen)
	vyRaw, _, _ := procGetSystemMetrics.Call(smYVirtualScreen)
	wRaw, _, _ := procGetSystemMetrics.Call(smCXVirtualScreen)
	hRaw, _, _ := procGetSystemMetrics.Call(smCYVirtualScreen)
	return MonitorBounds{
		Index:   -1,
		X:       int(int32(vxRaw)),
		Y:       int(int32(vyRaw)),
		Width:   int(int32(wRaw)),
		Height:  int(int32(hRaw)),
		Primary: false,
	}
}

// EnumMonitors returns all active monitors with Monitor 1 (index 0) guaranteed to be the primary monitor.
func EnumMonitors() []MonitorBounds {
	enumMonMu.Lock()
	enumMonList = enumMonList[:0]
	procEnumDisplayMonitors.Call(0, 0, enumMonCB, 0)
	list := make([]MonitorBounds, len(enumMonList))
	copy(list, enumMonList)
	enumMonMu.Unlock()

	if len(list) == 0 {
		vb := VirtualDesktopBounds()
		vb.Index = 0
		vb.Primary = true
		return []MonitorBounds{vb}
	}

	sort.Slice(list, func(i, j int) bool {
		if list[i].Primary != list[j].Primary {
			return list[i].Primary
		}
		if list[i].X != list[j].X {
			return list[i].X < list[j].X
		}
		return list[i].Y < list[j].Y
	})

	for i := range list {
		list[i].Index = i
	}
	return list
}

// ResolveMonitorBounds returns the capture/input bounds for monitorIndex:
// -1 = all monitors (virtual desktop), 0..N-1 = specific monitor (falls back to Monitor 1 if out of range).
func ResolveMonitorBounds(monitorIndex int) MonitorBounds {
	if monitorIndex < 0 {
		return VirtualDesktopBounds()
	}
	mons := EnumMonitors()
	if monitorIndex < len(mons) {
		return mons[monitorIndex]
	}
	return mons[0]
}
