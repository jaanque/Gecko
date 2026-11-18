//go:build darwin

package darwin

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

const clipTimeout = 1500 * time.Millisecond

// ReadClipboardText reads UTF-8 text from the macOS clipboard via pbpaste.
func ReadClipboardText() string {
	ctx, cancel := context.WithTimeout(context.Background(), clipTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "pbpaste").Output()
	if err != nil || len(out) == 0 {
		return ""
	}
	return string(out)
}

// WriteClipboardText writes UTF-8 text to the macOS clipboard via pbcopy.
func WriteClipboardText(text string) error {
	if text == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), clipTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pbcopy")
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}
