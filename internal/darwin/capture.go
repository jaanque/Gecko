//go:build darwin

package darwin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"remote-control/internal/video"
)

// ScreenCapturer captures the macOS desktop (with cursor) using built-in screencapture
// and encodes changed 64x64 tiles via the shared video.TileEncoder.
type ScreenCapturer struct {
	tmpPath  string
	currBGRA []byte
	encoder  *video.TileEncoder
}

// NewScreenCapturer creates a macOS ScreenCapturer.
func NewScreenCapturer() *ScreenCapturer {
	tmpPath := ""
	if f, err := os.CreateTemp("", "gecko-mac-*.bmp"); err == nil {
		tmpPath = f.Name()
		_ = f.Close()
	}
	return &ScreenCapturer{
		tmpPath: tmpPath,
		encoder: video.NewTileEncoder(),
	}
}

// Close cleans up temporary capture files and buffers.
func (sc *ScreenCapturer) Close() {
	if sc.tmpPath != "" {
		_ = os.Remove(sc.tmpPath)
		sc.tmpPath = ""
	}
	sc.currBGRA = nil
	if sc.encoder != nil {
		sc.encoder.Close()
	}
}

// CaptureFrame captures the macOS screen into BGRA and returns an encoded MsgFrame payload.
func (sc *ScreenCapturer) CaptureFrame(frameID uint32, monitorIndex, quality int, forceFull bool) ([]byte, int, int, error) {
	if sc.tmpPath == "" {
		f, err := os.CreateTemp("", "gecko-mac-*.bmp")
		if err != nil {
			return nil, 0, 0, err
		}
		sc.tmpPath = f.Name()
		_ = f.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	args := []string{"-x", "-C", "-t", "bmp"}
	if monitorIndex >= 0 {
		args = append(args, "-D", fmt.Sprintf("%d", monitorIndex+1))
	}
	args = append(args, sc.tmpPath)

	if err := exec.CommandContext(ctx, "screencapture", args...).Run(); err != nil {
		return nil, 0, 0, err
	}

	raw, err := os.ReadFile(sc.tmpPath)
	if err != nil {
		return nil, 0, 0, err
	}
	bgra, w, h, err := decodeBMPToBGRA(raw, sc.currBGRA)
	if err != nil {
		return nil, 0, 0, err
	}
	sc.currBGRA = bgra
	pkt := sc.encoder.Encode(frameID, sc.currBGRA, w, h, quality, forceFull)
	return pkt, w, h, nil
}
