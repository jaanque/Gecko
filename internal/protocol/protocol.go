package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	MaxPayloadSize  = 16 * 1024 * 1024 // 16 MiB hard cap per message
	HeaderSize      = 5
	FrameHeaderSize = 10 // frameID(4) + width(2) + height(2) + tileCount(2)
	TileHeaderSize  = 13 // x(2) + y(2) + w(2) + h(2) + codec(1) + dataLen(4)
)

// Message types.
const (
	MsgAuth         uint8 = 0x01
	MsgAuthResp     uint8 = 0x02
	MsgPing         uint8 = 0x03
	MsgPong         uint8 = 0x04
	MsgAdminAuth    uint8 = 0x05
	MsgClientList   uint8 = 0x06
	MsgConsentReq   uint8 = 0x10
	MsgConsentResp  uint8 = 0x11
	MsgSessionStop  uint8 = 0x12
	MsgStreamConfig uint8 = 0x13
	MsgAdminConnect uint8 = 0x14
	MsgSessionEvent uint8 = 0x15
	MsgFrame        uint8 = 0x20
	MsgFrameAck     uint8 = 0x21
	MsgMouse        uint8 = 0x30
	MsgKey          uint8 = 0x31
)

// Tile codecs inside MsgFrame payloads.
const (
	TileCodecJPEG    uint8 = 0
	TileCodecZlibBGR uint8 = 1
)

// Mouse actions.
const (
	MouseMove   uint8 = 0
	MouseDown   uint8 = 1
	MouseUp     uint8 = 2
	MouseWheel  uint8 = 3
	MouseHWheel uint8 = 4
)

// Mouse buttons.
const (
	ButtonLeft   uint8 = 0
	ButtonRight  uint8 = 1
	ButtonMiddle uint8 = 2
	ButtonX1     uint8 = 3
	ButtonX2     uint8 = 4
)

// Key actions & flags.
const (
	KeyDown         uint8 = 0
	KeyUp           uint8 = 1
	KeyFlagExtended uint8 = 1 << 0
)

// MonitorInfo describes one display attached to a client PC.
type MonitorInfo struct {
	Index   int  `json:"index"`
	Width   int  `json:"width"`
	Height  int  `json:"height"`
	Primary bool `json:"primary"`
}

// AuthPayload is sent by the client right after establishing the TLS connection.
type AuthPayload struct {
	Token    string        `json:"token"`
	Hostname string        `json:"hostname"`
	Width    int           `json:"width"`
	Height   int           `json:"height"`
	Monitors []MonitorInfo `json:"monitors,omitempty"`
}

// AuthRespPayload is sent by the admin in response to MsgAuth.
type AuthRespPayload struct {
	OK       bool   `json:"ok"`
	ClientID string `json:"client_id,omitempty"`
	Error    string `json:"error,omitempty"`
}

// AdminAuthPayload authenticates an Admin desktop app against the Relay server.
type AdminAuthPayload struct {
	Token string `json:"token"`
}

// ClientSummary describes an online client PC reported by the Relay to Admin.
type ClientSummary struct {
	ID           string `json:"id"`
	Hostname     string `json:"hostname"`
	IP           string `json:"ip"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	MonitorCount int    `json:"monitor_count"`
	Status       string `json:"status"` // "online", "pending", "active"
}

// AdminConnectPayload requests remote control of a specific online client via the Relay.
type AdminConnectPayload struct {
	ClientID string `json:"client_id"`
}

// SessionEventPayload notifies Admin of session lifecycle changes from the Relay.
type SessionEventPayload struct {
	Event        string `json:"event"` // "pending", "started", "denied", "ended", "error"
	SessionID    string `json:"session_id,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	Hostname     string `json:"hostname,omitempty"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
	MonitorCount int    `json:"monitor_count,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// ConsentReqPayload asks the client user to allow or deny remote control.
type ConsentReqPayload struct {
	SessionID  string `json:"session_id"`
	TimeoutSec int    `json:"timeout_sec"`
}

// ConsentRespPayload carries the client user's decision.
type ConsentRespPayload struct {
	SessionID string `json:"session_id"`
	Allowed   bool   `json:"allowed"`
	Reason    string `json:"reason,omitempty"`
}

// SessionStopPayload ends an active or pending session from either side.
type SessionStopPayload struct {
	SessionID string `json:"session_id"`
	Reason    string `json:"reason,omitempty"`
}

// StreamConfigPayload adjusts quality, target monitor, or requests a full keyframe.
type StreamConfigPayload struct {
	Quality  int  `json:"quality,omitempty"`  // 35..95
	Monitor  *int `json:"monitor,omitempty"`  // 0..N-1 for specific monitor, -1 for all
	Keyframe bool `json:"keyframe,omitempty"`
}

// MouseEvent is encoded as 8 bytes: action(1), button(1), x(2), y(2), delta(2).
type MouseEvent struct {
	Action uint8
	Button uint8
	X      uint16 // normalized 0..65535
	Y      uint16 // normalized 0..65535
	Delta  int16
}

// KeyEvent is encoded as 6 bytes: action(1), vk(2), scan(2), flags(1).
type KeyEvent struct {
	Action uint8
	VK     uint16
	Scan   uint16
	Flags  uint8
}

// EncodeFrameAck serializes a frame ID into 4 bytes.
func EncodeFrameAck(frameID uint32) []byte {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], frameID)
	return buf[:]
}

// DecodeFrameAck parses a 4-byte frame acknowledgment.
func DecodeFrameAck(b []byte) (uint32, error) {
	if len(b) != 4 {
		return 0, fmt.Errorf("invalid frame ack size: %d", len(b))
	}
	return binary.BigEndian.Uint32(b), nil
}

// EncodeMouse serializes a MouseEvent to 8 bytes.
func EncodeMouse(ev MouseEvent) []byte {
	var buf [8]byte
	buf[0] = ev.Action
	buf[1] = ev.Button
	binary.BigEndian.PutUint16(buf[2:4], ev.X)
	binary.BigEndian.PutUint16(buf[4:6], ev.Y)
	binary.BigEndian.PutUint16(buf[6:8], uint16(ev.Delta))
	return buf[:]
}

// DecodeMouse parses an 8-byte MouseEvent.
func DecodeMouse(b []byte) (MouseEvent, error) {
	if len(b) != 8 {
		return MouseEvent{}, fmt.Errorf("invalid mouse event size: %d", len(b))
	}
	return MouseEvent{
		Action: b[0],
		Button: b[1],
		X:      binary.BigEndian.Uint16(b[2:4]),
		Y:      binary.BigEndian.Uint16(b[4:6]),
		Delta:  int16(binary.BigEndian.Uint16(b[6:8])),
	}, nil
}

// EncodeKey serializes a KeyEvent to 6 bytes.
func EncodeKey(ev KeyEvent) []byte {
	var buf [6]byte
	buf[0] = ev.Action
	binary.BigEndian.PutUint16(buf[1:3], ev.VK)
	binary.BigEndian.PutUint16(buf[3:5], ev.Scan)
	buf[5] = ev.Flags
	return buf[:]
}

// DecodeKey parses a 6-byte KeyEvent.
func DecodeKey(b []byte) (KeyEvent, error) {
	if len(b) != 6 {
		return KeyEvent{}, fmt.Errorf("invalid key event size: %d", len(b))
	}
	return KeyEvent{
		Action: b[0],
		VK:     binary.BigEndian.Uint16(b[1:3]),
		Scan:   binary.BigEndian.Uint16(b[3:5]),
		Flags:  b[5],
	}, nil
}

// WriteMessage writes a length-prefixed binary message to w.
func WriteMessage(w io.Writer, msgType uint8, payload []byte) error {
	if len(payload) > MaxPayloadSize {
		return fmt.Errorf("payload exceeds max size (%d > %d)", len(payload), MaxPayloadSize)
	}
	var hdr [HeaderSize]byte
	hdr[0] = msgType
	binary.BigEndian.PutUint32(hdr[1:5], uint32(len(payload)))
	if err := writeAll(w, hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		return writeAll(w, payload)
	}
	return nil
}

// WriteJSON marshals v as JSON and writes it as a message.
func WriteJSON(w io.Writer, msgType uint8, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteMessage(w, msgType, data)
}

// ReadMessage reads one length-prefixed binary message from r.
func ReadMessage(r io.Reader) (uint8, []byte, error) {
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	msgType := hdr[0]
	length := binary.BigEndian.Uint32(hdr[1:5])
	if length > MaxPayloadSize {
		return 0, nil, errors.New("message payload exceeds limit")
	}
	if length == 0 {
		return msgType, nil, nil
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return msgType, payload, nil
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
