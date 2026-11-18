package adminweb

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

const (
	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xA

	wsGUID       = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	wsMaxReadMsg = 2 * 1024 * 1024 // 2 MiB max incoming control message from browser
)

// wsConn is a minimal, zero-dependency RFC 6455 server WebSocket connection.
type wsConn struct {
	conn    net.Conn
	br      *bufio.Reader
	writeMu sync.Mutex
}

func upgradeWebSocket(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, errors.New("missing Upgrade: websocket header")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		return nil, errors.New("missing Sec-WebSocket-Key")
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("hijacking unsupported")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}

	h := sha1.Sum([]byte(key + wsGUID))
	accept := base64.StdEncoding.EncodeToString(h[:])

	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		_ = conn.Close()
		return nil, err
	}

	return &wsConn{conn: conn, br: rw.Reader}, nil
}

func (c *wsConn) Close() error {
	return c.conn.Close()
}

func (c *wsConn) WriteFrame(opcode byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	var hdr [10]byte
	hdr[0] = 0x80 | (opcode & 0x0F) // FIN=1
	n := len(payload)
	var hlen int
	switch {
	case n <= 125:
		hdr[1] = byte(n)
		hlen = 2
	case n <= 65535:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(n))
		hlen = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(n))
		hlen = 10
	}

	if _, err := c.conn.Write(hdr[:hlen]); err != nil {
		return err
	}
	if n > 0 {
		_, err := c.conn.Write(payload)
		return err
	}
	return nil
}

func (c *wsConn) ReadMessage() (byte, []byte, error) {
	var msgOp byte
	var out []byte

	for {
		var h [2]byte
		if _, err := io.ReadFull(c.br, h[:]); err != nil {
			return 0, nil, err
		}
		fin := (h[0] & 0x80) != 0
		op := h[0] & 0x0F
		masked := (h[1] & 0x80) != 0
		plen := uint64(h[1] & 0x7F)

		switch plen {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return 0, nil, err
			}
			plen = uint64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return 0, nil, err
			}
			plen = binary.BigEndian.Uint64(ext[:])
		}

		if plen > wsMaxReadMsg || uint64(len(out))+plen > wsMaxReadMsg {
			return 0, nil, errors.New("websocket frame too large")
		}

		var maskKey [4]byte
		if masked {
			if _, err := io.ReadFull(c.br, maskKey[:]); err != nil {
				return 0, nil, err
			}
		}

		payload := make([]byte, plen)
		if plen > 0 {
			if _, err := io.ReadFull(c.br, payload); err != nil {
				return 0, nil, err
			}
			if masked {
				for i := range payload {
					payload[i] ^= maskKey[i%4]
				}
			}
		}

		switch op {
		case wsOpClose:
			_ = c.WriteFrame(wsOpClose, nil)
			return 0, nil, io.EOF
		case wsOpPing:
			_ = c.WriteFrame(wsOpPong, payload)
			continue
		case wsOpPong:
			continue
		case wsOpText, wsOpBinary:
			msgOp = op
			out = append(out, payload...)
		case wsOpContinuation:
			out = append(out, payload...)
		}

		if fin {
			return msgOp, out, nil
		}
	}
}
