package ws

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

const (
	OpContinuation = 0x0
	OpText         = 0x1
	OpBinary       = 0x2
	OpClose        = 0x8
	OpPing         = 0x9
	OpPong         = 0xA

	MaxMessageSize = 16 * 1024 * 1024
	wsGUID         = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

// Conn represents a server-side RFC 6455 WebSocket connection.
type Conn struct {
	netConn net.Conn
	br      *bufio.Reader
	wmu     sync.Mutex
	closed  bool
}

// Upgrade upgrades an HTTP request into a WebSocket connection.
func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !headerContainsToken(r.Header.Get("Connection"), "upgrade") ||
		!strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return nil, errors.New("not a websocket upgrade request")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		http.Error(w, "unsupported websocket version", http.StatusUpgradeRequired)
		return nil, errors.New("unsupported websocket version")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		http.Error(w, "missing websocket key", http.StatusBadRequest)
		return nil, errors.New("missing Sec-WebSocket-Key")
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return nil, errors.New("http.Hijacker not supported")
	}
	netConn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}

	accept := computeAcceptKey(key)
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		netConn.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		netConn.Close()
		return nil, err
	}

	return &Conn{
		netConn: netConn,
		br:      rw.Reader,
	}, nil
}

// ReadMessage reads one complete text or binary WebSocket message, handling control frames automatically.
func (c *Conn) ReadMessage() (byte, []byte, error) {
	var msgOp byte
	var msgBuf []byte

	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}

		switch op {
		case OpPing:
			_ = c.writeFrame(OpPong, payload)
			continue
		case OpPong:
			continue
		case OpClose:
			_ = c.writeFrame(OpClose, nil)
			return 0, nil, io.EOF
		case OpText, OpBinary:
			if msgOp != 0 {
				return 0, nil, errors.New("unexpected data frame during fragmented message")
			}
			msgOp = op
			msgBuf = append(msgBuf, payload...)
		case OpContinuation:
			if msgOp == 0 {
				return 0, nil, errors.New("unexpected continuation frame")
			}
			if len(msgBuf)+len(payload) > MaxMessageSize {
				return 0, nil, errors.New("message exceeds max size")
			}
			msgBuf = append(msgBuf, payload...)
		default:
			return 0, nil, fmt.Errorf("unsupported websocket opcode 0x%x", op)
		}

		if fin {
			return msgOp, msgBuf, nil
		}
	}
}

// WriteText sends a text WebSocket frame.
func (c *Conn) WriteText(data []byte) error {
	return c.writeFrame(OpText, data)
}

// WriteBinary sends a binary WebSocket frame.
func (c *Conn) WriteBinary(data []byte) error {
	return c.writeFrame(OpBinary, data)
}

// Close closes the underlying WebSocket connection.
func (c *Conn) Close() error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	_ = c.writeFrameLocked(OpClose, nil)
	return c.netConn.Close()
}

func (c *Conn) readFrame() (bool, byte, []byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return false, 0, nil, err
	}
	fin := (hdr[0] & 0x80) != 0
	op := hdr[0] & 0x0F
	masked := (hdr[1] & 0x80) != 0
	if !masked {
		return false, 0, nil, errors.New("client websocket frame must be masked")
	}

	var length uint64 = uint64(hdr[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if length > MaxMessageSize {
		return false, 0, nil, errors.New("websocket frame exceeds max size")
	}

	var mask [4]byte
	if _, err := io.ReadFull(c.br, mask[:]); err != nil {
		return false, 0, nil, err
	}

	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return false, 0, nil, err
		}
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return fin, op, payload, nil
}

func (c *Conn) writeFrame(op byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	return c.writeFrameLocked(op, payload)
}

func (c *Conn) writeFrameLocked(op byte, payload []byte) error {
	var hdr [10]byte
	hdr[0] = 0x80 | (op & 0x0F)
	n := len(payload)
	hdrLen := 2
	switch {
	case n <= 125:
		hdr[1] = byte(n)
	case n <= 65535:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
		hdrLen = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
		hdrLen = 10
	}

	if err := writeFull(c.netConn, hdr[:hdrLen]); err != nil {
		return err
	}
	if n > 0 {
		return writeFull(c.netConn, payload)
	}
	return nil
}

func writeFull(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

func computeAcceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key))
	h.Write([]byte(wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func headerContainsToken(header, token string) bool {
	for _, part := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}
