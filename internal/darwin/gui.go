//go:build darwin

package darwin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"remote-control/internal/clientui"
	"remote-control/internal/i18n"
)

// ClientWindow manages the native macOS consent prompt and lifecycle loop.
type ClientWindow struct {
	hostname  string
	strings   i18n.ClientStrings
	callbacks clientui.Callbacks

	mu        sync.Mutex
	state     clientui.State
	detail    string
	stateCh   chan clientui.State
	readyCh   chan struct{}
	readyOnce sync.Once
}

// NewClientWindow constructs a macOS ClientWindow instance.
func NewClientWindow(hostname string, _ []byte, lang i18n.Lang, cb clientui.Callbacks) *ClientWindow {
	str := i18n.Client(lang)
	return &ClientWindow{
		hostname:  hostname,
		strings:   str,
		callbacks: cb,
		state:     clientui.StateConnecting,
		detail:    str.Connecting,
		stateCh:   make(chan clientui.State, 8),
		readyCh:   make(chan struct{}),
	}
}

// Ready returns a channel closed once the client UI loop is running.
func (w *ClientWindow) Ready() <-chan struct{} {
	return w.readyCh
}

// SetState updates state and notifies the macOS prompt handler.
func (w *ClientWindow) SetState(st clientui.State, detail string) {
	w.mu.Lock()
	w.state = st
	w.detail = detail
	w.mu.Unlock()

	if detail != "" {
		fmt.Printf("[%s] %s\n", w.strings.WindowTitle, detail)
	}
	select {
	case w.stateCh <- st:
	default:
	}
}

// Run displays native macOS consent dialogs when requested and waits for SIGINT/SIGTERM.
func (w *ClientWindow) Run() error {
	w.readyOnce.Do(func() { close(w.readyCh) })
	fmt.Printf("[%s] %s (macOS)\n", w.strings.WindowTitle, w.hostname)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	for {
		select {
		case <-sigCh:
			if w.callbacks.OnClose != nil {
				w.callbacks.OnClose()
			}
			return nil
		case st := <-w.stateCh:
			if st == clientui.StatePrompt {
				go func() {
					if askMacConsent(w.strings.WindowTitle+" — "+w.hostname, w.strings.PromptMessage, w.strings.Allow, w.strings.Deny) {
						if w.callbacks.OnAllow != nil {
							w.callbacks.OnAllow()
						}
					} else if w.callbacks.OnDeny != nil {
						w.callbacks.OnDeny()
					}
				}()
			}
		}
	}
}

func askMacConsent(title, msg, allowBtn, denyBtn string) bool {
	esc := func(s string) string {
		s = strings.ReplaceAll(s, `\`, `\\`)
		return strings.ReplaceAll(s, `"`, `\"`)
	}
	script := fmt.Sprintf(
		`button returned of (display dialog "%s" with title "%s" buttons {"%s", "%s"} default button "%s" giving up after 55)`,
		esc(msg), esc(title), esc(denyBtn), esc(allowBtn), esc(allowBtn),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 58*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "osascript", "-e", script).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == allowBtn
}
