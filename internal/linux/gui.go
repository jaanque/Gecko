//go:build linux

package linux

import (
	"encoding/binary"
	"fmt"
	"image"
	"io"
	"os"
	"sync"

	"remote-control/internal/clientui"
)

const (
	xEventMaskExposure      = 0x00008000
	xEventMaskButtonPress   = 0x00000004
	xEventMaskButtonRelease = 0x00000008
	xEventMaskPointerMotion = 0x00000040
	xEventMaskLeaveWindow   = 0x00000020
	xEventMaskSubstructNot  = 0x00080000
	xEventMaskSubstructRed  = 0x00100000

	xEvExpose        = 12
	xEvLeaveNotify   = 8
	xEvClientMessage = 33
)

// ClientWindow manages the native pure-Go X11 client window (Catalan UI).
type ClientWindow struct {
	hostname  string
	logoPNG   []byte
	callbacks clientui.Callbacks

	mu        sync.Mutex
	state     clientui.State
	detail    string
	xc        *x11Conn
	win       uint32
	gc        uint32
	atomWake  uint32
	readyCh   chan struct{}
	readyOnce sync.Once
	closed    bool

	canvas  *clientCanvas
	hover   uiButtonID
	pressed uiButtonID
	topmost bool
}

// NewClientWindow constructs a Linux X11 ClientWindow instance.
func NewClientWindow(hostname string, logoPNG []byte, cb clientui.Callbacks) *ClientWindow {
	return &ClientWindow{
		hostname:  hostname,
		logoPNG:   logoPNG,
		callbacks: cb,
		state:     clientui.StateConnecting,
		detail:    "Connectant...",
		readyCh:   make(chan struct{}),
		canvas:    newClientCanvas(logoPNG),
	}
}

// Ready returns a channel closed once the X11 window has been created.
func (w *ClientWindow) Ready() <-chan struct{} {
	return w.readyCh
}

// SetState safely updates the window state from any goroutine.
func (w *ClientWindow) SetState(st clientui.State, detail string) {
	w.mu.Lock()
	w.state = st
	w.detail = detail
	xc := w.xc
	win := w.win
	atomWake := w.atomWake
	closed := w.closed
	w.mu.Unlock()

	if !closed && xc != nil && win != 0 && atomWake != 0 {
		_ = xc.SendClientMessage(win, win, atomWake, 0, 0, 0, 0, 0, 0)
	}
}

// Run creates the native X11 window and processes events until closed.
func (w *ClientWindow) Run() error {
	xc, err := dialX11()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Client Remot: %v\n", err)
		w.readyOnce.Do(func() { close(w.readyCh) })
		return err
	}
	defer func() {
		w.mu.Lock()
		w.closed = true
		w.xc = nil
		w.mu.Unlock()
		_ = xc.Close()
	}()

	atomWMProtocols, _ := xc.InternAtom("WM_PROTOCOLS", false)
	atomWMDelete, _ := xc.InternAtom("WM_DELETE_WINDOW", false)
	atomNetWMName, _ := xc.InternAtom("_NET_WM_NAME", false)
	atomUTF8String, _ := xc.InternAtom("UTF8_STRING", false)
	atomNetWMState, _ := xc.InternAtom("_NET_WM_STATE", false)
	atomNetWMStateAbove, _ := xc.InternAtom("_NET_WM_STATE_ABOVE", false)
	atomNetActiveWindow, _ := xc.InternAtom("_NET_ACTIVE_WINDOW", false)
	atomNetWMIcon, _ := xc.InternAtom("_NET_WM_ICON", false)
	atomWake, _ := xc.InternAtom("_REMOTE_CLIENT_WAKE", false)

	win := xc.NextXID()
	gc := xc.NextXID()

	posX := int16(max(20, (int(xc.rootW)-clientWinW)/2))
	posY := int16(max(20, (int(xc.rootH)-clientWinH)/2))
	evMask := uint32(xEventMaskExposure | xEventMaskButtonPress | xEventMaskButtonRelease | xEventMaskPointerMotion | xEventMaskLeaveWindow)
	if err := xc.CreateWindow(win, xc.root, posX, posY, clientWinW, clientWinH, evMask, xc.whitePixel); err != nil {
		w.readyOnce.Do(func() { close(w.readyCh) })
		return err
	}
	if err := xc.CreateGC(gc, win); err != nil {
		w.readyOnce.Do(func() { close(w.readyCh) })
		return err
	}

	// Title: WM_NAME (atom 39, type STRING = 31) and _NET_WM_NAME (UTF8_STRING).
	titleBytes := []byte("Client Remot")
	_ = xc.ChangeProperty(win, 39, 31, 8, titleBytes)
	if atomNetWMName != 0 && atomUTF8String != 0 {
		_ = xc.ChangeProperty(win, atomNetWMName, atomUTF8String, 8, titleBytes)
	}

	// Fixed window size via WM_NORMAL_HINTS (atom 40, type WM_SIZE_HINTS = 41).
	var hints [18 * 4]byte
	const pMinSize = 1 << 4
	const pMaxSize = 1 << 5
	binary.LittleEndian.PutUint32(hints[0:4], pMinSize|pMaxSize)
	binary.LittleEndian.PutUint32(hints[5*4:6*4], clientWinW) // min_width
	binary.LittleEndian.PutUint32(hints[6*4:7*4], clientWinH) // min_height
	binary.LittleEndian.PutUint32(hints[7*4:8*4], clientWinW) // max_width
	binary.LittleEndian.PutUint32(hints[8*4:9*4], clientWinH) // max_height
	_ = xc.ChangeProperty(win, 40, 41, 32, hints[:])

	// Register WM_DELETE_WINDOW protocol.
	if atomWMProtocols != 0 && atomWMDelete != 0 {
		var delBuf [4]byte
		binary.LittleEndian.PutUint32(delBuf[:], atomWMDelete)
		_ = xc.ChangeProperty(win, atomWMProtocols, 4, 32, delBuf[:]) // type ATOM = 4
	}

	// Optional taskbar/titlebar icon (_NET_WM_ICON, type CARDINAL = 6).
	if atomNetWMIcon != 0 && w.canvas.logo != nil {
		if iconData := buildNetWMIcon(w.canvas.logo); len(iconData) > 0 {
			_ = xc.ChangeProperty(win, atomNetWMIcon, 6, 32, iconData)
		}
	}

	_ = xc.MapWindow(win)

	w.mu.Lock()
	w.xc = xc
	w.win = win
	w.gc = gc
	w.atomWake = atomWake
	w.mu.Unlock()

	w.paint(xc, win, gc)
	w.readyOnce.Do(func() { close(w.readyCh) })

	var ev [32]byte
	for {
		if _, err := io.ReadFull(xc.conn, ev[:]); err != nil {
			break
		}
		evType := ev[0] & 0x7F
		switch evType {
		case xEvExpose:
			// Repaint on final expose count == 0.
			if binary.LittleEndian.Uint16(ev[16:18]) == 0 {
				w.paint(xc, win, gc)
			}

		case xEventMotionNotify:
			mx := int(int16(binary.LittleEndian.Uint16(ev[24:26])))
			my := int(int16(binary.LittleEndian.Uint16(ev[26:28])))
			st := w.currentState()
			hit := w.canvas.hitTest(st, mx, my)
			if hit != w.hover {
				w.hover = hit
				w.paint(xc, win, gc)
			}

		case xEvLeaveNotify:
			if w.hover != btnNone {
				w.hover = btnNone
				w.paint(xc, win, gc)
			}

		case xEventButtonPress:
			if ev[1] == 1 { // Left click
				mx := int(int16(binary.LittleEndian.Uint16(ev[24:26])))
				my := int(int16(binary.LittleEndian.Uint16(ev[26:28])))
				w.pressed = w.canvas.hitTest(w.currentState(), mx, my)
			}

		case xEventButtonRelease:
			if ev[1] == 1 {
				mx := int(int16(binary.LittleEndian.Uint16(ev[24:26])))
				my := int(int16(binary.LittleEndian.Uint16(ev[26:28])))
				hit := w.canvas.hitTest(w.currentState(), mx, my)
				pressed := w.pressed
				w.pressed = btnNone
				if hit != btnNone && hit == pressed {
					w.dispatchButton(hit)
				}
			}

		case xEvClientMessage:
			msgType := binary.LittleEndian.Uint32(ev[8:12])
			d0 := binary.LittleEndian.Uint32(ev[12:16])
			if msgType == atomWMProtocols && d0 == atomWMDelete {
				if w.callbacks.OnClose != nil {
					w.callbacks.OnClose()
				}
				_ = xc.DestroyWindow(win)
				return nil
			}
			if msgType == atomWake {
				st := w.currentState()
				if st == clientui.StatePrompt {
					_ = xc.MapWindow(win)
					_ = xc.RaiseWindow(win)
					if atomNetWMState != 0 && atomNetWMStateAbove != 0 {
						_ = xc.SendClientMessage(xc.root, win, atomNetWMState, xEventMaskSubstructRed|xEventMaskSubstructNot, 1, atomNetWMStateAbove, 0, 1, 0)
						w.topmost = true
					}
					if atomNetActiveWindow != 0 {
						_ = xc.SendClientMessage(xc.root, win, atomNetActiveWindow, xEventMaskSubstructRed|xEventMaskSubstructNot, 1, 0, 0, 0, 0)
					}
				} else if w.topmost {
					if atomNetWMState != 0 && atomNetWMStateAbove != 0 {
						_ = xc.SendClientMessage(xc.root, win, atomNetWMState, xEventMaskSubstructRed|xEventMaskSubstructNot, 0, atomNetWMStateAbove, 0, 1, 0)
					}
					w.topmost = false
				}
				w.hover = btnNone
				w.pressed = btnNone
				w.paint(xc, win, gc)
			}

		default:
			if evType == 35 { // GenericEvent with extra length
				extraLen := int(binary.LittleEndian.Uint32(ev[4:8])) * 4
				if extraLen > 0 {
					_, _ = io.CopyN(io.Discard, xc.conn, int64(extraLen))
				}
			}
		}
	}

	if w.callbacks.OnClose != nil {
		w.callbacks.OnClose()
	}
	return nil
}

func (w *ClientWindow) currentState() clientui.State {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state
}

func (w *ClientWindow) paint(xc *x11Conn, win, gc uint32) {
	w.mu.Lock()
	st := w.state
	detail := w.detail
	w.mu.Unlock()

	w.canvas.render(w.hostname, st, detail, w.hover)
	_ = xc.PutImageStriped(win, gc, w.canvas.w, w.canvas.h, w.canvas.bgra)
}

func (w *ClientWindow) dispatchButton(btn uiButtonID) {
	switch btn {
	case btnAllow:
		if w.callbacks.OnAllow != nil {
			go w.callbacks.OnAllow()
		}
	case btnDeny:
		if w.callbacks.OnDeny != nil {
			go w.callbacks.OnDeny()
		}
	case btnDisconnect:
		if w.callbacks.OnDisconnect != nil {
			go w.callbacks.OnDisconnect()
		}
	}
}

func buildNetWMIcon(src *image.NRGBA) []byte {
	if src == nil {
		return nil
	}
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return nil
	}
	const sz = 48
	out := make([]byte, (2+sz*sz)*4)
	binary.LittleEndian.PutUint32(out[0:4], sz)
	binary.LittleEndian.PutUint32(out[4:8], sz)
	off := 8
	for y := 0; y < sz; y++ {
		sy := (y * sh) / sz
		for x := 0; x < sz; x++ {
			sx := (x * sw) / sz
			c := src.NRGBAAt(sx, sy)
			argb := uint32(c.A)<<24 | uint32(c.R)<<16 | uint32(c.G)<<8 | uint32(c.B)
			binary.LittleEndian.PutUint32(out[off:off+4], argb)
			off += 4
		}
	}
	return out
}
