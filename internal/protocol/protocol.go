package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	MaxPayloadSize          = 16 * 1024 * 1024 // 16 MiB hard cap for MsgFrame
	MaxControlPayloadSize   = 16 * 1024        // 16 KiB hard cap for JSON control messages
	MaxListPayloadSize      = 512 * 1024       // 512 KiB hard cap for MsgClientList
	MaxPingPayloadSize      = 64               // 64 bytes hard cap for MsgPing/MsgPong
	MaxClipboardPayloadSize = 256 * 1024       // 256 KiB hard cap for MsgClipboard
	MaxFileChunkPayloadSize = 260 * 1024       // 256 KiB chunk data + binary header
	FileChunkSize           = 128 * 1024       // 128 KiB per file transfer chunk
	MaxFileTransferSize     = 50 * 1024 * 1024 // 50 MiB max file transfer
	HeaderSize              = 5
	FrameHeaderSize         = 10 // frameID(4) + width(2) + height(2) + tileCount(2)
	TileHeaderSize          = 13 // x(2) + y(2) + w(2) + h(2) + codec(1) + dataLen(4)
)

// Message types.
const (
	MsgAuth                 uint8 = 0x01
	MsgAuthResp             uint8 = 0x02
	MsgPing                 uint8 = 0x03
	MsgPong                 uint8 = 0x04
	MsgAdminAuth            uint8 = 0x05
	MsgClientList           uint8 = 0x06
	MsgAdminProvisionClient uint8 = 0x07
	MsgAdminProvisionResp   uint8 = 0x08
	MsgAdminRevokeClient    uint8 = 0x09
	MsgConsentReq           uint8 = 0x10
	MsgConsentResp          uint8 = 0x11
	MsgSessionStop          uint8 = 0x12
	MsgStreamConfig         uint8 = 0x13
	MsgAdminConnect         uint8 = 0x14
	MsgSessionEvent         uint8 = 0x15
	MsgElevateReq           uint8 = 0x16
	MsgFrame                uint8 = 0x20
	MsgFrameAck             uint8 = 0x21
	MsgCursor               uint8 = 0x22
	MsgMouse                uint8 = 0x30
	MsgKey                  uint8 = 0x31
	MsgClipboard            uint8 = 0x32
	MsgSystemAction         uint8 = 0x33
	MsgFileOffer            uint8 = 0x40
	MsgFileAccept           uint8 = 0x41
	MsgFileChunk            uint8 = 0x42
	MsgFileDone             uint8 = 0x43
	MsgP2POffer             uint8 = 0x50
	MsgP2PAnswer            uint8 = 0x51
)

// System actions.
const (
	ActionCtrlAltDel = "ctrl_alt_del"
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
	Username string        `json:"username,omitempty"`
	Version  string        `json:"version,omitempty"`
	Width    int           `json:"width"`
	Height   int           `json:"height"`
	Monitors []MonitorInfo `json:"monitors,omitempty"`
}

// AuthRespPayload is sent by the relay in response to MsgAuth / MsgAdminAuth.
type AuthRespPayload struct {
	OK       bool   `json:"ok"`
	ClientID string `json:"client_id,omitempty"`
	Error    string `json:"error,omitempty"`
}

// AdminAuthPayload authenticates an Admin desktop app against the Relay server.
type AdminAuthPayload struct {
	Token string `json:"token"`
}

// AdminProvisionReqPayload requests a fresh per-client mTLS credential from the Relay.
type AdminProvisionReqPayload struct {
	Label string `json:"label,omitempty"`
}

// AdminProvisionRespPayload carries a freshly minted revocable client credential.
type AdminProvisionRespPayload struct {
	OK            bool   `json:"ok"`
	CredentialID  string `json:"credential_id,omitempty"`
	ClientToken   string `json:"client_token,omitempty"`
	ClientCertDER []byte `json:"client_cert_der,omitempty"`
	ClientKeyDER  []byte `json:"client_key_der,omitempty"`
	Error         string `json:"error,omitempty"`
}

// AdminRevokeClientPayload asks the Relay to revoke a client credential and disconnect it.
type AdminRevokeClientPayload struct {
	CredentialID string `json:"credential_id,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
}

// ClientSummary describes an online client PC reported by the Relay to Admin.
type ClientSummary struct {
	ID           string `json:"id"`
	CredentialID string `json:"credential_id,omitempty"`
	Hostname     string `json:"hostname"`
	Username     string `json:"username,omitempty"`
	Version      string `json:"version,omitempty"`
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
	Quality  int  `json:"quality,omitempty"` // 35..95
	Monitor  *int `json:"monitor,omitempty"` // 0..N-1 for specific monitor, -1 for all
	Keyframe bool `json:"keyframe,omitempty"`
	Auto     bool `json:"auto,omitempty"`     // true to let dynamic adaptive controller adjust quality & FPS
}

// ClipboardPayload synchronizes UTF-8 clipboard text during an active session.
type ClipboardPayload struct {
	Text string `json:"text"`
}

// SystemActionPayload triggers a privileged/system action on the client during an active session.
type SystemActionPayload struct {
	Action string `json:"action"` // e.g. ActionCtrlAltDel
}

// FileOfferPayload initiates a file transfer from Admin to Client during an active session.
type FileOfferPayload struct {
	TransferID string `json:"transfer_id"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
}

// FileAcceptPayload acknowledges readiness (or rejection) to receive file chunks.
type FileAcceptPayload struct {
	TransferID string `json:"transfer_id"`
	Accepted   bool   `json:"accepted"`
	Reason     string `json:"reason,omitempty"`
}

// FileDonePayload signals completion (or error) of a file transfer.
type FileDonePayload struct {
	TransferID string `json:"transfer_id"`
	OK         bool   `json:"ok"`
	SavedPath  string `json:"saved_path,omitempty"`
	Error      string `json:"error,omitempty"`
}

// ElevateReqPayload requests the client to elevate via UAC or restart as an elevated service agent.
type ElevateReqPayload struct {
	SessionID string `json:"session_id"`
}

// P2POfferPayload advertises direct LAN / public listener endpoints for peer-to-peer connection.
type P2POfferPayload struct {
	SessionID string   `json:"session_id"`
	Endpoints []string `json:"endpoints"`
	Token     string   `json:"token"`
}

// P2PAnswerPayload responds to a P2POfferPayload indicating direct connection outcome.
type P2PAnswerPayload struct {
	SessionID string `json:"session_id"`
	OK        bool   `json:"ok"`
	Endpoint  string `json:"endpoint,omitempty"`
}

const (
	MaxCursorPayloadSize = 64 * 1024
	CursorHeaderSize     = 18

	CursorFlagVisible  uint8 = 1 << 0
	CursorFlagHasShape uint8 = 1 << 1
)

// CursorPacket carries decoupled mouse cursor position and shape information.
type CursorPacket struct {
	X         uint16
	Y         uint16
	HotspotX  uint16
	HotspotY  uint16
	Width     uint16
	Height    uint16
	Visible   bool
	ShapeHash uint32
	ShapeBGRA []byte
}

// EncodeCursor encodes a CursorPacket into a compact binary representation.
func EncodeCursor(p CursorPacket) ([]byte, error) {
	flags := uint8(0)
	if p.Visible {
		flags |= CursorFlagVisible
	}
	hasShape := len(p.ShapeBGRA) > 0
	if hasShape {
		flags |= CursorFlagHasShape
	}

	total := CursorHeaderSize
	if hasShape {
		total += 4 + len(p.ShapeBGRA)
	}
	if total > MaxCursorPayloadSize {
		return nil, errors.New("cursor packet exceeds max payload size")
	}

	buf := make([]byte, total)
	binary.BigEndian.PutUint16(buf[0:2], p.X)
	binary.BigEndian.PutUint16(buf[2:4], p.Y)
	binary.BigEndian.PutUint16(buf[4:6], p.HotspotX)
	binary.BigEndian.PutUint16(buf[6:8], p.HotspotY)
	binary.BigEndian.PutUint16(buf[8:10], p.Width)
	binary.BigEndian.PutUint16(buf[10:12], p.Height)
	buf[12] = flags
	buf[13] = 0 // reserved
	binary.BigEndian.PutUint32(buf[14:18], p.ShapeHash)

	if hasShape {
		binary.BigEndian.PutUint32(buf[18:22], uint32(len(p.ShapeBGRA)))
		copy(buf[22:], p.ShapeBGRA)
	}
	return buf, nil
}

// DecodeCursor decodes a CursorPacket from raw bytes.
func DecodeCursor(b []byte) (CursorPacket, error) {
	if len(b) < CursorHeaderSize {
		return CursorPacket{}, errors.New("cursor payload too short")
	}
	p := CursorPacket{
		X:         binary.BigEndian.Uint16(b[0:2]),
		Y:         binary.BigEndian.Uint16(b[2:4]),
		HotspotX:  binary.BigEndian.Uint16(b[4:6]),
		HotspotY:  binary.BigEndian.Uint16(b[6:8]),
		Width:     binary.BigEndian.Uint16(b[8:10]),
		Height:    binary.BigEndian.Uint16(b[10:12]),
		Visible:   (b[12] & CursorFlagVisible) != 0,
		ShapeHash: binary.BigEndian.Uint32(b[14:18]),
	}
	hasShape := (b[12] & CursorFlagHasShape) != 0
	if hasShape {
		if len(b) < CursorHeaderSize+4 {
			return CursorPacket{}, errors.New("cursor payload truncated shape length")
		}
		dataLen := int(binary.BigEndian.Uint32(b[18:22]))
		if dataLen > MaxCursorPayloadSize || CursorHeaderSize+4+dataLen > len(b) {
			return CursorPacket{}, errors.New("cursor payload truncated shape data")
		}
		p.ShapeBGRA = b[22 : 22+dataLen]
	}
	return p, nil
}

// FileChunk carries one binary chunk of a file transfer.
type FileChunk struct {
	TransferID string
	Offset     uint64
	Data       []byte
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

// EncodeFileChunk serializes a FileChunk as: idLen(1) + id + offset(8) + data.
func EncodeFileChunk(ch FileChunk) ([]byte, error) {
	if len(ch.TransferID) == 0 || len(ch.TransferID) > 64 {
		return nil, errors.New("invalid transfer_id length")
	}
	total := 1 + len(ch.TransferID) + 8 + len(ch.Data)
	if total > MaxFileChunkPayloadSize {
		return nil, errors.New("file chunk exceeds max payload size")
	}
	buf := make([]byte, total)
	buf[0] = byte(len(ch.TransferID))
	copy(buf[1:], ch.TransferID)
	offPos := 1 + len(ch.TransferID)
	binary.BigEndian.PutUint64(buf[offPos:offPos+8], ch.Offset)
	copy(buf[offPos+8:], ch.Data)
	return buf, nil
}

// DecodeFileChunk parses and validates a binary FileChunk payload.
func DecodeFileChunk(b []byte) (FileChunk, error) {
	if len(b) < 10 {
		return FileChunk{}, errors.New("file chunk too short")
	}
	idLen := int(b[0])
	if idLen == 0 || idLen > 64 || len(b) < 1+idLen+8 {
		return FileChunk{}, errors.New("invalid file chunk header")
	}
	id := string(b[1 : 1+idLen])
	offPos := 1 + idLen
	offset := binary.BigEndian.Uint64(b[offPos : offPos+8])
	return FileChunk{
		TransferID: id,
		Offset:     offset,
		Data:       b[offPos+8:],
	}, nil
}

// SanitizeFileName strips any directory path, traversal, and control/invalid characters.
func SanitizeFileName(raw string) string {
	s := strings.ReplaceAll(strings.TrimSpace(raw), "\\", "/")
	s = filepath.Base(s)
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= 180 {
			break
		}
	}
	out := strings.TrimSpace(strings.Trim(b.String(), "."))
	if out == "" {
		return "archivo.bin"
	}
	return out
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

// DecodeMouse parses and strictly validates an 8-byte MouseEvent.
func DecodeMouse(b []byte) (MouseEvent, error) {
	if len(b) != 8 {
		return MouseEvent{}, fmt.Errorf("invalid mouse event size: %d", len(b))
	}
	ev := MouseEvent{
		Action: b[0],
		Button: b[1],
		X:      binary.BigEndian.Uint16(b[2:4]),
		Y:      binary.BigEndian.Uint16(b[4:6]),
		Delta:  int16(binary.BigEndian.Uint16(b[6:8])),
	}
	if ev.Action > MouseHWheel {
		return MouseEvent{}, fmt.Errorf("invalid mouse action: %d", ev.Action)
	}
	if ev.Button > ButtonX2 {
		return MouseEvent{}, fmt.Errorf("invalid mouse button: %d", ev.Button)
	}
	return ev, nil
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

// DecodeKey parses and strictly validates a 6-byte KeyEvent.
func DecodeKey(b []byte) (KeyEvent, error) {
	if len(b) != 6 {
		return KeyEvent{}, fmt.Errorf("invalid key event size: %d", len(b))
	}
	ev := KeyEvent{
		Action: b[0],
		VK:     binary.BigEndian.Uint16(b[1:3]),
		Scan:   binary.BigEndian.Uint16(b[3:5]),
		Flags:  b[5],
	}
	if ev.Action > KeyUp {
		return KeyEvent{}, fmt.Errorf("invalid key action: %d", ev.Action)
	}
	if ev.VK == 0 || ev.VK > 0xFE {
		return KeyEvent{}, fmt.Errorf("invalid virtual key: 0x%X", ev.VK)
	}
	if (ev.Flags &^ KeyFlagExtended) != 0 {
		return KeyEvent{}, fmt.Errorf("invalid key flags: 0x%X", ev.Flags)
	}
	return ev, nil
}

// MaxPayloadForType returns the maximum permitted payload size for a known message type.
// Unknown message types return (0, false) and are rejected before allocation.
func MaxPayloadForType(msgType uint8) (uint32, bool) {
	switch msgType {
	case MsgAuth, MsgAuthResp, MsgAdminAuth, MsgAdminConnect,
		MsgAdminProvisionClient, MsgAdminProvisionResp, MsgAdminRevokeClient,
		MsgConsentReq, MsgConsentResp, MsgSessionStop,
		MsgStreamConfig, MsgSessionEvent, MsgSystemAction,
		MsgFileOffer, MsgFileAccept, MsgFileDone,
		MsgElevateReq, MsgP2POffer, MsgP2PAnswer:
		return MaxControlPayloadSize, true
	case MsgClientList:
		return MaxListPayloadSize, true
	case MsgPing, MsgPong:
		return MaxPingPayloadSize, true
	case MsgClipboard:
		return MaxClipboardPayloadSize, true
	case MsgCursor:
		return MaxCursorPayloadSize, true
	case MsgFileChunk:
		return MaxFileChunkPayloadSize, true
	case MsgFrame:
		return MaxPayloadSize, true
	case MsgFrameAck:
		return 4, true
	case MsgMouse:
		return 8, true
	case MsgKey:
		return 6, true
	default:
		return 0, false
	}
}

// WriteMessage writes a length-prefixed binary message to w after verifying type and size limits.
func WriteMessage(w io.Writer, msgType uint8, payload []byte) error {
	limit, ok := MaxPayloadForType(msgType)
	if !ok {
		return fmt.Errorf("unknown message type: 0x%02X", msgType)
	}
	if uint64(len(payload)) > uint64(limit) {
		return fmt.Errorf("payload exceeds max size for type 0x%02X (%d > %d)", msgType, len(payload), limit)
	}
	var hdr [HeaderSize]byte
	hdr[0] = msgType
	binary.BigEndian.PutUint32(hdr[1:5], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := w.Write(payload)
		return err
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

// ReadMessage reads one length-prefixed binary message from r, strictly enforcing
// known message types and per-type payload size limits before allocating memory.
func ReadMessage(r io.Reader) (uint8, []byte, error) {
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	msgType := hdr[0]
	limit, ok := MaxPayloadForType(msgType)
	if !ok {
		return 0, nil, fmt.Errorf("unknown message type: 0x%02X", msgType)
	}
	length := binary.BigEndian.Uint32(hdr[1:5])
	if length > limit {
		return 0, nil, errors.New("message payload exceeds limit for type")
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
