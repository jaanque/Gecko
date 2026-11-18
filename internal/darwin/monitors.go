//go:build darwin

package darwin

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"remote-control/internal/clientui"
)

// EnumMonitors queries macOS CoreGraphics via built-in JXA to determine primary display dimensions.
func EnumMonitors() []clientui.MonitorBounds {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "osascript", "-l", "JavaScript", "-e",
		`ObjC.import("CoreGraphics"); JSON.stringify({w: Number($.CGDisplayPixelsWide($.CGMainDisplayID())), h: Number($.CGDisplayPixelsHigh($.CGMainDisplayID()))})`,
	).Output()
	if err == nil {
		var res struct {
			W json.Number `json:"w"`
			H json.Number `json:"h"`
		}
		dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(string(out))))
		dec.UseNumber()
		if dec.Decode(&res) == nil {
			w, _ := strconv.Atoi(res.W.String())
			h, _ := strconv.Atoi(res.H.String())
			if w > 0 && h > 0 {
				return []clientui.MonitorBounds{{
					Index:   0,
					X:       0,
					Y:       0,
					Width:   w,
					Height:  h,
					Primary: true,
				}}
			}
		}
	}
	return []clientui.MonitorBounds{{
		Index:   0,
		Width:   1440,
		Height:  900,
		Primary: true,
	}}
}
