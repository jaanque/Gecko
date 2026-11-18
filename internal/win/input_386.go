//go:build windows && 386

package win

type mouseInputPacket struct {
	Type        uint32
	DX          int32
	DY          int32
	MouseData   uint32
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}

type keybdInputPacket struct {
	Type        uint32
	WVk         uint16
	WScan       uint16
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
	_           [8]byte
}
