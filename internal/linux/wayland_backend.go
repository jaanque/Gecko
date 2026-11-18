//go:build linux

package linux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/png"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"remote-control/internal/protocol"
)

// waylandBackend provides screen capture and input injection on Linux Wayland compositors
// using standard Wayland utilities (grim, spectacle, gnome-screenshot, ydotool, wtype)
// while keeping zero external Go dependencies (CGO_ENABLED=0).
type waylandBackend struct {
	captureTool string // "grim", "spectacle", "gnome-screenshot", ""
	ydotoolPath string
	wtypePath   string
	checked     bool
}

func (wb *waylandBackend) detect() {
	if wb.checked {
		return
	}
	wb.checked = true
	for _, tool := range []string{"grim", "spectacle", "gnome-screenshot"} {
		if _, err := exec.LookPath(tool); err == nil {
			wb.captureTool = tool
			break
		}
	}
	if p, err := exec.LookPath("ydotool"); err == nil {
		wb.ydotoolPath = p
	}
	if p, err := exec.LookPath("wtype"); err == nil {
		wb.wtypePath = p
	}
}

func (wb *waylandBackend) canCapture() bool {
	wb.detect()
	return wb.captureTool != ""
}

func (wb *waylandBackend) captureBGRA(dst []byte) ([]byte, int, int, error) {
	wb.detect()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	switch wb.captureTool {
	case "grim":
		// PPM (P6) avoids PNG compression overhead for much higher FPS on wlroots/Sway/Hyprland.
		out, err := exec.CommandContext(ctx, "grim", "-t", "ppm", "-").Output()
		if err != nil {
			return dst, 0, 0, err
		}
		return decodePPMToBGRA(out, dst)

	case "spectacle", "gnome-screenshot":
		f, err := os.CreateTemp("", "gecko-wl-*.png")
		if err != nil {
			return dst, 0, 0, err
		}
		tmpPath := f.Name()
		_ = f.Close()
		defer os.Remove(tmpPath)

		var cmd *exec.Cmd
		if wb.captureTool == "spectacle" {
			cmd = exec.CommandContext(ctx, "spectacle", "-b", "-n", "-f", "-o", tmpPath)
		} else {
			cmd = exec.CommandContext(ctx, "gnome-screenshot", "-f", tmpPath)
		}
		if err := cmd.Run(); err != nil {
			return dst, 0, 0, err
		}
		raw, err := os.ReadFile(tmpPath)
		if err != nil {
			return dst, 0, 0, err
		}
		img, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			return dst, 0, 0, err
		}
		return imageToBGRA(img, dst)

	default:
		return dst, 0, 0, errors.New("cap eina de captura Wayland trobada (instal·la grim, spectacle o gnome-screenshot)")
	}
}

func (wb *waylandBackend) injectMouse(ev protocol.MouseEvent, w, h int) bool {
	wb.detect()
	if wb.ydotoolPath == "" || w <= 1 || h <= 1 {
		return false
	}
	px := int((float64(ev.X)/65535.0)*float64(w-1) + 0.5)
	py := int((float64(ev.Y)/65535.0)*float64(h-1) + 0.5)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	switch ev.Action {
	case protocol.MouseMove:
		_ = exec.CommandContext(ctx, wb.ydotoolPath, "mousemove", "-a", "-x", strconv.Itoa(px), "-y", strconv.Itoa(py)).Run()
	case protocol.MouseDown:
		_ = exec.CommandContext(ctx, wb.ydotoolPath, "mousemove", "-a", "-x", strconv.Itoa(px), "-y", strconv.Itoa(py)).Run()
		_ = exec.CommandContext(ctx, wb.ydotoolPath, "click", ydotoolButtonCode(ev.Button, true)).Run()
	case protocol.MouseUp:
		_ = exec.CommandContext(ctx, wb.ydotoolPath, "click", ydotoolButtonCode(ev.Button, false)).Run()
	case protocol.MouseWheel:
		steps := int(ev.Delta) / 120
		if steps == 0 {
			if ev.Delta > 0 {
				steps = 1
			} else if ev.Delta < 0 {
				steps = -1
			}
		}
		_ = exec.CommandContext(ctx, wb.ydotoolPath, "mousemove", "-w", "-x", "0", "-y", strconv.Itoa(steps)).Run()
	}
	return true
}

func ydotoolButtonCode(btn uint8, down bool) string {
	// ydotool click codes: 0x40=down, 0x80=up; low bits: 0=left, 1=right, 2=middle
	var base byte = 0x00
	switch btn {
	case protocol.ButtonRight:
		base = 0x01
	case protocol.ButtonMiddle:
		base = 0x02
	}
	if down {
		return fmt.Sprintf("0x%02X", 0x40|base)
	}
	return fmt.Sprintf("0x%02X", 0x80|base)
}

func (wb *waylandBackend) injectKey(ev protocol.KeyEvent) bool {
	wb.detect()
	if wb.ydotoolPath == "" {
		return false
	}
	ext := (ev.Flags & protocol.KeyFlagExtended) != 0
	evdevKC := int(ScanToEvdevKeycode(ev.Scan, ext)) - 8 // Linux evdev code = X11 keycode - 8
	if evdevKC <= 0 {
		return false
	}
	state := "0"
	if ev.Action == protocol.KeyDown {
		state = "1"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = exec.CommandContext(ctx, wb.ydotoolPath, "key", fmt.Sprintf("%d:%s", evdevKC, state)).Run()
	return true
}

func decodePPMToBGRA(ppm, dst []byte) ([]byte, int, int, error) {
	if len(ppm) < 12 || ppm[0] != 'P' || ppm[1] != '6' {
		return dst, 0, 0, errors.New("format PPM invàlid")
	}
	pos := 2
	readToken := func() (string, bool) {
		for pos < len(ppm) {
			if ppm[pos] == '#' {
				for pos < len(ppm) && ppm[pos] != '\n' {
					pos++
				}
			} else if ppm[pos] == ' ' || ppm[pos] == '\t' || ppm[pos] == '\r' || ppm[pos] == '\n' {
				pos++
			} else {
				break
			}
		}
		start := pos
		for pos < len(ppm) && ppm[pos] != ' ' && ppm[pos] != '\t' && ppm[pos] != '\r' && ppm[pos] != '\n' {
			pos++
		}
		return string(ppm[start:pos]), pos > start
	}

	wStr, ok1 := readToken()
	hStr, ok2 := readToken()
	maxStr, ok3 := readToken()
	if !ok1 || !ok2 || !ok3 || pos >= len(ppm) {
		return dst, 0, 0, errors.New("capçalera PPM incompleta")
	}
	pos++ // single whitespace separator after maxval
	w, err1 := strconv.Atoi(wStr)
	h, err2 := strconv.Atoi(hStr)
	maxVal, err3 := strconv.Atoi(maxStr)
	if err1 != nil || err2 != nil || err3 != nil || w <= 0 || h <= 0 || maxVal != 255 {
		return dst, 0, 0, errors.New("dimensions PPM invàlides")
	}
	pix := ppm[pos:]
	if len(pix) < w*h*3 {
		return dst, 0, 0, errors.New("dades PPM truncades")
	}
	needed := w * h * 4
	if cap(dst) < needed {
		dst = make([]byte, needed)
	} else {
		dst = dst[:needed]
	}
	p := 0
	for i := 0; i < w*h*4; i += 4 {
		dst[i+0] = pix[p+2] // B
		dst[i+1] = pix[p+1] // G
		dst[i+2] = pix[p+0] // R
		dst[i+3] = 255
		p += 3
	}
	return dst, w, h, nil
}

func imageToBGRA(img image.Image, dst []byte) ([]byte, int, int, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return dst, 0, 0, errors.New("imatge buida")
	}
	needed := w * h * 4
	if cap(dst) < needed {
		dst = make([]byte, needed)
	} else {
		dst = dst[:needed]
	}
	off := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			dst[off+0] = byte(bl >> 8)
			dst[off+1] = byte(g >> 8)
			dst[off+2] = byte(r >> 8)
			dst[off+3] = 255
			off += 4
		}
	}
	return dst, w, h, nil
}

// showWaylandConsentDialog displays a native GTK/Qt consent prompt when running on pure Wayland
// without XWayland (DISPLAY empty). Returns (allowed, handled).
func showWaylandConsentDialog(title, msg, allowLabel, denyLabel string) (bool, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()

	if p, err := exec.LookPath("zenity"); err == nil {
		cmd := exec.CommandContext(ctx, p, "--question",
			"--title="+title,
			"--text="+msg,
			"--ok-label="+allowLabel,
			"--cancel-label="+denyLabel,
		)
		return cmd.Run() == nil, true
	}
	if p, err := exec.LookPath("kdialog"); err == nil {
		cmd := exec.CommandContext(ctx, p, "--title", title,
			"--yesno", msg,
			"--yes-label", allowLabel,
			"--no-label", denyLabel,
		)
		return cmd.Run() == nil, true
	}
	_ = strings.TrimSpace(title)
	return false, false
}
