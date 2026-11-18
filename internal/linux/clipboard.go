//go:build linux

package linux

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

const clipboardTimeout = 1500 * time.Millisecond

// ReadClipboardText reads UTF-8 text from the Linux X11 clipboard using xclip or xsel if installed.
func ReadClipboardText() string {
	ctx, cancel := context.WithTimeout(context.Background(), clipboardTimeout)
	defer cancel()

	var cmd *exec.Cmd
	if _, err := exec.LookPath("xclip"); err == nil {
		cmd = exec.CommandContext(ctx, "xclip", "-o", "-selection", "clipboard")
	} else if _, err := exec.LookPath("xsel"); err == nil {
		cmd = exec.CommandContext(ctx, "xsel", "--clipboard", "--output")
	} else {
		return ""
	}

	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return ""
	}
	return string(out)
}

// WriteClipboardText writes UTF-8 text to the Linux X11 clipboard using xclip or xsel if installed.
func WriteClipboardText(text string) error {
	if text == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), clipboardTimeout)
	defer cancel()

	var cmd *exec.Cmd
	if _, err := exec.LookPath("xclip"); err == nil {
		cmd = exec.CommandContext(ctx, "xclip", "-i", "-selection", "clipboard")
	} else if _, err := exec.LookPath("xsel"); err == nil {
		cmd = exec.CommandContext(ctx, "xsel", "--clipboard", "--input")
	} else {
		return errors.New("xclip/xsel no disponible")
	}

	cmd.Stdin = strings.NewReader(text)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	return cmd.Run()
}
