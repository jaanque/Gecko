//go:build linux

package linux

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type x11Conn struct {
	conn net.Conn

	mu    sync.Mutex // serializes synchronous request/reply operations
	seq   uint16
	xidMu sync.Mutex
	xidN  uint32

	resourceBase uint32
	resourceMask uint32
	maxReqLen    uint32 // in 4-byte units

	root          uint32
	rootVisual    uint32
	rootDepth     byte
	rootBPP       byte
	rootW         uint16
	rootH         uint16
	whitePixel    uint32
	blackPixel    uint32
	imageOrderMSB bool
	redMask       uint32
	greenMask     uint32
	blueMask      uint32
	minKeycode    byte
	maxKeycode    byte
}

func dialX11() (*x11Conn, error) {
	display := os.Getenv("DISPLAY")
	if display == "" {
		return nil, errors.New("no s'ha trobat cap sessió gràfica X11 (DISPLAY buit)")
	}

	netConn, dispNum, screenIdx, err := connectDisplay(display)
	if err != nil {
		return nil, fmt.Errorf("no es pot connectar a X11 (%s): %w", display, err)
	}

	authName, authData := readXAuthority(dispNum)

	xc := &x11Conn{conn: netConn}
	if err := xc.handshake(authName, authData, screenIdx); err != nil {
		_ = netConn.Close()
		return nil, err
	}
	return xc, nil
}

func (c *x11Conn) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func connectDisplay(display string) (net.Conn, string, int, error) {
	// Support direct unix socket paths (e.g. /tmp/launch-.../org.x:0).
	if strings.HasPrefix(display, "/") {
		c, err := net.DialTimeout("unix", display, 3*time.Second)
		return c, "0", 0, err
	}

	colon := strings.LastIndex(display, ":")
	if colon < 0 {
		return nil, "", 0, fmt.Errorf("DISPLAY invàlid: %s", display)
	}
	host := display[:colon]
	rest := display[colon+1:]

	dispNumStr := rest
	screenIdx := 0
	if dot := strings.Index(rest, "."); dot >= 0 {
		dispNumStr = rest[:dot]
		if n, err := strconv.Atoi(rest[dot+1:]); err == nil && n >= 0 {
			screenIdx = n
		}
	}
	dispNum, err := strconv.Atoi(dispNumStr)
	if err != nil || dispNum < 0 {
		return nil, "", 0, fmt.Errorf("número de DISPLAY invàlid: %s", display)
	}

	if host == "" || host == "unix" || host == "localhost" {
		sockPath := fmt.Sprintf("/tmp/.X11-unix/X%d", dispNum)
		if c, err := net.DialTimeout("unix", sockPath, 3*time.Second); err == nil {
			return c, dispNumStr, screenIdx, nil
		}
		// Fallback to Linux abstract Unix socket namespace (@/tmp/.X11-unix/X<N>).
		if c, err := net.DialTimeout("unix", "\x00"+sockPath, 3*time.Second); err == nil {
			return c, dispNumStr, screenIdx, nil
		}
		if host == "" || host == "unix" {
			return nil, "", 0, fmt.Errorf("no es pot obrir el sòcol X11 %s", sockPath)
		}
	}

	c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, 6000+dispNum), 3*time.Second)
	return c, dispNumStr, screenIdx, err
}

func readXAuthority(dispNum string) (string, []byte) {
	path := os.Getenv("XAUTHORITY")
	if path == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			path = filepath.Join(home, ".Xauthority")
		}
	}
	if path == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil
	}
	hostname, _ := os.Hostname()

	var fallbackName string
	var fallbackData []byte

	pos := 0
	readBytes := func() ([]byte, bool) {
		if pos+2 > len(data) {
			return nil, false
		}
		n := int(binary.BigEndian.Uint16(data[pos : pos+2]))
		pos += 2
		if pos+n > len(data) {
			return nil, false
		}
		b := data[pos : pos+n]
		pos += n
		return b, true
	}

	for pos+2 <= len(data) {
		family := binary.BigEndian.Uint16(data[pos : pos+2])
		pos += 2
		addr, ok1 := readBytes()
		num, ok2 := readBytes()
		name, ok3 := readBytes()
		cookie, ok4 := readBytes()
		if !ok1 || !ok2 || !ok3 || !ok4 {
			break
		}
		if string(name) != "MIT-MAGIC-COOKIE-1" {
			continue
		}
		numStr := string(num)
		if numStr != "" && numStr != dispNum {
			continue
		}
		// Exact match on FamilyWild (65535) or FamilyLocal (256) with matching hostname.
		if family == 65535 || (family == 256 && (len(addr) == 0 || string(addr) == hostname)) {
			return string(name), append([]byte(nil), cookie...)
		}
		if fallbackName == "" {
			fallbackName = string(name)
			fallbackData = append([]byte(nil), cookie...)
		}
	}
	return fallbackName, fallbackData
}

func pad4(n int) int {
	return (n + 3) &^ 3
}

func (c *x11Conn) handshake(authName string, authData []byte, screenIdx int) error {
	_ = c.conn.SetDeadline(time.Now().Add(5 * time.Second))
	defer c.conn.SetDeadline(time.Time{})

	namePad := pad4(len(authName))
	dataPad := pad4(len(authData))
	req := make([]byte, 12+namePad+dataPad)
	req[0] = 'l' // LSB first (little-endian)
	binary.LittleEndian.PutUint16(req[2:4], 11)
	binary.LittleEndian.PutUint16(req[4:6], 0)
	binary.LittleEndian.PutUint16(req[6:8], uint16(len(authName)))
	binary.LittleEndian.PutUint16(req[8:10], uint16(len(authData)))
	copy(req[12:], authName)
	copy(req[12+namePad:], authData)

	if _, err := c.conn.Write(req); err != nil {
		return err
	}

	var hdr [8]byte
	if _, err := io.ReadFull(c.conn, hdr[:]); err != nil {
		return err
	}
	extraLen := int(binary.LittleEndian.Uint16(hdr[6:8])) * 4
	body := make([]byte, extraLen)
	if _, err := io.ReadFull(c.conn, body); err != nil {
		return err
	}

	if hdr[0] != 1 {
		reasonLen := int(hdr[1])
		if reasonLen > len(body) {
			reasonLen = len(body)
		}
		return fmt.Errorf("autenticació X11 rebutjada: %s", string(body[:reasonLen]))
	}
	if len(body) < 32 {
		return errors.New("resposta X11 incompleta")
	}

	c.resourceBase = binary.LittleEndian.Uint32(body[4:8])
	c.resourceMask = binary.LittleEndian.Uint32(body[8:12])
	vendorLen := int(binary.LittleEndian.Uint16(body[16:18]))
	c.maxReqLen = uint32(binary.LittleEndian.Uint16(body[18:20]))
	numRoots := int(body[20])
	numFormats := int(body[21])
	c.imageOrderMSB = body[22] == 1
	c.minKeycode = body[26]
	c.maxKeycode = body[27]

	pos := 32 + pad4(vendorLen)
	formatBPP := make(map[byte]byte, numFormats)
	for i := 0; i < numFormats && pos+8 <= len(body); i++ {
		formatBPP[body[pos]] = body[pos+1]
		pos += 8
	}

	if numRoots == 0 {
		return errors.New("X11 no té cap pantalla")
	}
	if screenIdx >= numRoots {
		screenIdx = 0
	}

	for s := 0; s < numRoots && pos+40 <= len(body); s++ {
		root := binary.LittleEndian.Uint32(body[pos : pos+4])
		white := binary.LittleEndian.Uint32(body[pos+8 : pos+12])
		black := binary.LittleEndian.Uint32(body[pos+12 : pos+16])
		w := binary.LittleEndian.Uint16(body[pos+20 : pos+22])
		h := binary.LittleEndian.Uint16(body[pos+22 : pos+24])
		rootVis := binary.LittleEndian.Uint32(body[pos+32 : pos+36])
		rootDepth := body[pos+38]
		numDepths := int(body[pos+39])
		pos += 40

		var rMask, gMask, bMask uint32
		for d := 0; d < numDepths && pos+8 <= len(body); d++ {
			numVis := int(binary.LittleEndian.Uint16(body[pos+2 : pos+4]))
			pos += 8
			for v := 0; v < numVis && pos+24 <= len(body); v++ {
				vid := binary.LittleEndian.Uint32(body[pos : pos+4])
				if vid == rootVis {
					rMask = binary.LittleEndian.Uint32(body[pos+8 : pos+12])
					gMask = binary.LittleEndian.Uint32(body[pos+12 : pos+16])
					bMask = binary.LittleEndian.Uint32(body[pos+16 : pos+20])
				}
				pos += 24
			}
		}

		if s == screenIdx {
			c.root = root
			c.whitePixel = white
			c.blackPixel = black
			c.rootW = w
			c.rootH = h
			c.rootVisual = rootVis
			c.rootDepth = rootDepth
			c.rootBPP = formatBPP[rootDepth]
			if c.rootBPP == 0 {
				c.rootBPP = 32
			}
			c.redMask = rMask
			c.greenMask = gMask
			c.blueMask = bMask
		}
	}

	if c.root == 0 {
		return errors.New("no s'ha pogut llegir la pantalla X11")
	}
	return nil
}

func (c *x11Conn) NextXID() uint32 {
	c.xidMu.Lock()
	defer c.xidMu.Unlock()
	c.xidN++
	return c.resourceBase | (c.xidN & c.resourceMask)
}

func (c *x11Conn) writeRaw(req []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	_, err := c.conn.Write(req)
	return err
}

// roundTrip sends an X11 request that expects a reply and reads until the matching reply or error.
// Intended for connections that do not run a concurrent event loop.
func (c *x11Conn) roundTrip(req []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.seq++
	wantSeq := c.seq

	if _, err := c.conn.Write(req); err != nil {
		return nil, err
	}

	var hdr [32]byte
	for {
		if _, err := io.ReadFull(c.conn, hdr[:]); err != nil {
			return nil, err
		}
		respType := hdr[0]
		seq := binary.LittleEndian.Uint16(hdr[2:4])
		switch respType {
		case 0: // Error
			errCode := hdr[1]
			majorOp := hdr[10]
			if seq == wantSeq {
				return nil, fmt.Errorf("error X11 codi=%d op=%d", errCode, majorOp)
			}
		case 1: // Reply
			extraLen := int(binary.LittleEndian.Uint32(hdr[4:8])) * 4
			buf := make([]byte, 32+extraLen)
			copy(buf[:32], hdr[:])
			if extraLen > 0 {
				if _, err := io.ReadFull(c.conn, buf[32:]); err != nil {
					return nil, err
				}
			}
			if seq == wantSeq {
				return buf, nil
			}
		default:
			// Discard any unsolicited 32-byte event (and GenericEvent extra words if type==35).
			if (respType & 0x7F) == 35 {
				extraLen := int(binary.LittleEndian.Uint32(hdr[4:8])) * 4
				if extraLen > 0 {
					_, _ = io.CopyN(io.Discard, c.conn, int64(extraLen))
				}
			}
		}
	}
}

func (c *x11Conn) QueryExtension(name string) (present bool, majorOpcode, firstEvent, firstError byte, err error) {
	n := len(name)
	pad := pad4(n)
	req := make([]byte, 8+pad)
	req[0] = 98
	binary.LittleEndian.PutUint16(req[2:4], uint16(len(req)/4))
	binary.LittleEndian.PutUint16(req[4:6], uint16(n))
	copy(req[8:], name)

	rep, err := c.roundTrip(req)
	if err != nil {
		return false, 0, 0, 0, err
	}
	return rep[8] != 0, rep[9], rep[10], rep[11], nil
}

func (c *x11Conn) InternAtom(name string, onlyIfExists bool) (uint32, error) {
	n := len(name)
	pad := pad4(n)
	req := make([]byte, 8+pad)
	req[0] = 16
	if onlyIfExists {
		req[1] = 1
	}
	binary.LittleEndian.PutUint16(req[2:4], uint16(len(req)/4))
	binary.LittleEndian.PutUint16(req[4:6], uint16(n))
	copy(req[8:], name)

	rep, err := c.roundTrip(req)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(rep[8:12]), nil
}

func (c *x11Conn) GetKeyboardMapping() (keysymsPerKeycode int, syms []uint32, err error) {
	count := int(c.maxKeycode) - int(c.minKeycode) + 1
	if count <= 0 || count > 255 {
		return 0, nil, errors.New("rang de tecles X11 invàlid")
	}
	var req [8]byte
	req[0] = 101
	binary.LittleEndian.PutUint16(req[2:4], 2)
	req[4] = c.minKeycode
	req[5] = byte(count)

	rep, err := c.roundTrip(req[:])
	if err != nil {
		return 0, nil, err
	}
	per := int(rep[1])
	raw := rep[32:]
	out := make([]uint32, len(raw)/4)
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(raw[i*4 : i*4+4])
	}
	return per, out, nil
}

func (c *x11Conn) GetImage(drawable uint32, x, y int16, w, h uint16) ([]byte, error) {
	var req [20]byte
	req[0] = 73
	req[1] = 2 // ZPixmap
	binary.LittleEndian.PutUint16(req[2:4], 5)
	binary.LittleEndian.PutUint32(req[4:8], drawable)
	binary.LittleEndian.PutUint16(req[8:10], uint16(x))
	binary.LittleEndian.PutUint16(req[10:12], uint16(y))
	binary.LittleEndian.PutUint16(req[12:14], w)
	binary.LittleEndian.PutUint16(req[14:16], h)
	binary.LittleEndian.PutUint32(req[16:20], 0xFFFFFFFF)

	rep, err := c.roundTrip(req[:])
	if err != nil {
		return nil, err
	}
	return rep[32:], nil
}

func (c *x11Conn) QueryPointer(win uint32) (rootX, rootY int16, err error) {
	var req [8]byte
	req[0] = 38
	binary.LittleEndian.PutUint16(req[2:4], 2)
	binary.LittleEndian.PutUint32(req[4:8], win)

	rep, err := c.roundTrip(req[:])
	if err != nil {
		return 0, 0, err
	}
	return int16(binary.LittleEndian.Uint16(rep[16:18])), int16(binary.LittleEndian.Uint16(rep[18:20])), nil
}

func (c *x11Conn) CreateWindow(wid, parent uint32, x, y int16, w, h uint16, eventMask uint32, bgPixel uint32) error {
	// ValueMask: CWBackPixel (0x0002) | CWEventMask (0x0800)
	var req [40]byte
	req[0] = 1
	req[1] = c.rootDepth
	binary.LittleEndian.PutUint16(req[2:4], 10)
	binary.LittleEndian.PutUint32(req[4:8], wid)
	binary.LittleEndian.PutUint32(req[8:12], parent)
	binary.LittleEndian.PutUint16(req[12:14], uint16(x))
	binary.LittleEndian.PutUint16(req[14:16], uint16(y))
	binary.LittleEndian.PutUint16(req[16:18], w)
	binary.LittleEndian.PutUint16(req[18:20], h)
	binary.LittleEndian.PutUint16(req[20:22], 0) // border width
	binary.LittleEndian.PutUint16(req[22:24], 1) // InputOutput
	binary.LittleEndian.PutUint32(req[24:28], c.rootVisual)
	binary.LittleEndian.PutUint32(req[28:32], 0x00000002|0x00000800)
	binary.LittleEndian.PutUint32(req[32:36], bgPixel)
	binary.LittleEndian.PutUint32(req[36:40], eventMask)
	return c.writeRaw(req[:])
}

func (c *x11Conn) CreateGC(gcid, drawable uint32) error {
	// GCGraphicsExposures (0x10000) = 0 so PutImage never floods NoExpose events.
	var req [20]byte
	req[0] = 55
	binary.LittleEndian.PutUint16(req[2:4], 5)
	binary.LittleEndian.PutUint32(req[4:8], gcid)
	binary.LittleEndian.PutUint32(req[8:12], drawable)
	binary.LittleEndian.PutUint32(req[12:16], 0x00010000)
	binary.LittleEndian.PutUint32(req[16:20], 0)
	return c.writeRaw(req[:])
}

func (c *x11Conn) ChangeProperty(win, prop, typ uint32, format byte, data []byte) error {
	pad := pad4(len(data))
	req := make([]byte, 24+pad)
	req[0] = 18
	req[1] = 0 // Replace
	binary.LittleEndian.PutUint16(req[2:4], uint16(len(req)/4))
	binary.LittleEndian.PutUint32(req[4:8], win)
	binary.LittleEndian.PutUint32(req[8:12], prop)
	binary.LittleEndian.PutUint32(req[12:16], typ)
	req[16] = format
	numItems := uint32(len(data)) / uint32(format/8)
	binary.LittleEndian.PutUint32(req[20:24], numItems)
	copy(req[24:], data)
	return c.writeRaw(req)
}

func (c *x11Conn) MapWindow(win uint32) error {
	var req [8]byte
	req[0] = 8
	binary.LittleEndian.PutUint16(req[2:4], 2)
	binary.LittleEndian.PutUint32(req[4:8], win)
	return c.writeRaw(req[:])
}

func (c *x11Conn) RaiseWindow(win uint32) error {
	// ConfigureWindow: CWStackMode (0x0040) = Above (0)
	var req [16]byte
	req[0] = 12
	binary.LittleEndian.PutUint16(req[2:4], 4)
	binary.LittleEndian.PutUint32(req[4:8], win)
	binary.LittleEndian.PutUint16(req[8:10], 0x0040)
	binary.LittleEndian.PutUint32(req[12:16], 0)
	return c.writeRaw(req[:])
}

func (c *x11Conn) DestroyWindow(win uint32) error {
	var req [8]byte
	req[0] = 4
	binary.LittleEndian.PutUint16(req[2:4], 2)
	binary.LittleEndian.PutUint32(req[4:8], win)
	return c.writeRaw(req[:])
}

func (c *x11Conn) SendClientMessage(destWin, targetWin, msgType, eventMask uint32, d0, d1, d2, d3, d4 uint32) error {
	var req [44]byte
	req[0] = 25 // SendEvent
	req[1] = 0  // propagate=false
	binary.LittleEndian.PutUint16(req[2:4], 11)
	binary.LittleEndian.PutUint32(req[4:8], destWin)
	binary.LittleEndian.PutUint32(req[8:12], eventMask)

	// 32-byte ClientMessage event (type 33, format 32).
	ev := req[12:44]
	ev[0] = 33
	ev[1] = 32
	binary.LittleEndian.PutUint32(ev[4:8], targetWin)
	binary.LittleEndian.PutUint32(ev[8:12], msgType)
	binary.LittleEndian.PutUint32(ev[12:16], d0)
	binary.LittleEndian.PutUint32(ev[16:20], d1)
	binary.LittleEndian.PutUint32(ev[20:24], d2)
	binary.LittleEndian.PutUint32(ev[24:28], d3)
	binary.LittleEndian.PutUint32(ev[28:32], d4)
	return c.writeRaw(req[:])
}

// PutImageStriped uploads a top-down 32-bit BGRA image in small horizontal strips so
// every X11 PutImage request fits comfortably within the core 256 KB request size limit.
func (c *x11Conn) PutImageStriped(drawable, gc uint32, w, h int, bgra []byte) error {
	if w <= 0 || h <= 0 || len(bgra) < w*h*4 {
		return nil
	}
	bppBytes := int(c.rootBPP) / 8
	if bppBytes != 4 && bppBytes != 3 {
		bppBytes = 4
	}
	maxBytes := 120 * 1024
	if c.maxReqLen > 0 {
		limit := int(c.maxReqLen)*4 - 64
		if limit > 0 && limit < maxBytes {
			maxBytes = limit
		}
	}
	rowBytes := w * bppBytes
	stripH := maxBytes / rowBytes
	if stripH < 1 {
		stripH = 1
	}

	for y := 0; y < h; y += stripH {
		sh := stripH
		if y+sh > h {
			sh = h - y
		}
		pixLen := sh * rowBytes
		pad := pad4(pixLen)
		req := make([]byte, 24+pad)
		req[0] = 72 // PutImage
		req[1] = 2  // ZPixmap
		binary.LittleEndian.PutUint16(req[2:4], uint16(len(req)/4))
		binary.LittleEndian.PutUint32(req[4:8], drawable)
		binary.LittleEndian.PutUint32(req[8:12], gc)
		binary.LittleEndian.PutUint16(req[12:14], uint16(w))
		binary.LittleEndian.PutUint16(req[14:16], uint16(sh))
		binary.LittleEndian.PutUint16(req[16:18], 0)
		binary.LittleEndian.PutUint16(req[18:20], uint16(int16(y)))
		req[20] = 0
		req[21] = c.rootDepth

		dst := req[24 : 24+pixLen]
		src := bgra[y*w*4 : (y+sh)*w*4]
		if bppBytes == 4 && !c.imageOrderMSB && (c.redMask == 0x00FF0000 || c.redMask == 0) {
			// Standard little-endian Xorg 24/32-bit TrueColor: B, G, R, X matches BGRA directly.
			copy(dst, src)
		} else {
			convertBGRAToX11(dst, src, w*sh, bppBytes, c.imageOrderMSB, c.redMask, c.greenMask, c.blueMask)
		}

		if err := c.writeRaw(req); err != nil {
			return err
		}
	}
	return nil
}

func convertBGRAToX11(dst, src []byte, count, bppBytes int, msb bool, rMask, gMask, bMask uint32) {
	rShift := maskShift(rMask, 16)
	gShift := maskShift(gMask, 8)
	bShift := maskShift(bMask, 0)
	for i := 0; i < count; i++ {
		b := uint32(src[i*4+0])
		g := uint32(src[i*4+1])
		r := uint32(src[i*4+2])
		px := (r << rShift) | (g << gShift) | (b << bShift)
		if bppBytes == 4 {
			if msb {
				binary.BigEndian.PutUint32(dst[i*4:i*4+4], px)
			} else {
				binary.LittleEndian.PutUint32(dst[i*4:i*4+4], px)
			}
		} else {
			if msb {
				dst[i*3+0] = byte(px >> 16)
				dst[i*3+1] = byte(px >> 8)
				dst[i*3+2] = byte(px)
			} else {
				dst[i*3+0] = byte(px)
				dst[i*3+1] = byte(px >> 8)
				dst[i*3+2] = byte(px >> 16)
			}
		}
	}
}

func maskShift(mask uint32, def uint) uint {
	if mask == 0 {
		return def
	}
	var s uint
	for (mask & 1) == 0 {
		mask >>= 1
		s++
	}
	return s
}
