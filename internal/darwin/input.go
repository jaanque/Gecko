//go:build darwin

package darwin

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"

	"remote-control/internal/protocol"
)

const jxaInputScript = `
ObjC.import("CoreGraphics");
ObjC.import("Foundation");
var stdin = $.NSFileHandle.fileHandleWithStandardInput;
var buf = "";
while (true) {
  var data = stdin.availableData;
  if (!data || data.length === 0) break;
  buf += ObjC.unwrap($.NSString.alloc.initWithDataEncoding(data, $.NSUTF8StringEncoding));
  var lines = buf.split("\n");
  buf = lines.pop();
  for (var i = 0; i < lines.length; i++) {
    var line = lines[i].trim();
    if (!line) continue;
    var p = line.split(" ");
    var cmd = p[0];
    if (cmd === "M") {
      var x = Number(p[1]), y = Number(p[2]);
      var ev = $.CGEventCreateMouseEvent(null, $.kCGEventMouseMoved, {x: x, y: y}, $.kCGMouseButtonLeft);
      $.CGEventPost($.kCGHIDEventTap, ev);
    } else if (cmd === "D" || cmd === "U") {
      var btn = Number(p[1]), x = Number(p[2]), y = Number(p[3]);
      var evType = cmd === "D" ? $.kCGEventLeftMouseDown : $.kCGEventLeftMouseUp;
      var cgBtn = $.kCGMouseButtonLeft;
      if (btn === 1) { evType = cmd === "D" ? $.kCGEventRightMouseDown : $.kCGEventRightMouseUp; cgBtn = $.kCGMouseButtonRight; }
      else if (btn === 2) { evType = cmd === "D" ? $.kCGEventOtherMouseDown : $.kCGEventOtherMouseUp; cgBtn = $.kCGMouseButtonCenter; }
      var ev = $.CGEventCreateMouseEvent(null, evType, {x: x, y: y}, cgBtn);
      $.CGEventPost($.kCGHIDEventTap, ev);
    } else if (cmd === "W") {
      var dy = Number(p[1]);
      var ev = $.CGEventCreateScrollWheelEvent(null, $.kCGScrollEventUnitLine, 1, dy);
      $.CGEventPost($.kCGHIDEventTap, ev);
    } else if (cmd === "K") {
      var kc = Number(p[1]), down = p[2] === "1";
      var ev = $.CGEventCreateKeyboardEvent(null, kc, down);
      $.CGEventPost($.kCGHIDEventTap, ev);
    }
  }
}
`

// InputInjector injects mouse and keyboard events on macOS using a persistent JXA CoreGraphics
// bridge (requires Accessibility permission in System Settings -> Privacy & Security).
type InputInjector struct {
	enabled atomic.Bool
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	w       *bufio.Writer
	screenW int
	screenH int
	held    map[uint16]bool
}

// NewInputInjector creates a macOS InputInjector (locked/disabled by default).
func NewInputInjector() *InputInjector {
	mons := EnumMonitors()
	sw, sh := 1440, 900
	if len(mons) > 0 {
		sw, sh = mons[0].Width, mons[0].Height
	}
	return &InputInjector{
		screenW: sw,
		screenH: sh,
		held:    make(map[uint16]bool),
	}
}

// SetEnabled opens or locks OS input injection.
func (inj *InputInjector) SetEnabled(enabled bool) {
	inj.enabled.Store(enabled)
	if !enabled {
		inj.ReleaseAllKeys()
		inj.stopWorker()
	}
}

func (inj *InputInjector) ensureWorkerLocked() bool {
	if inj.cmd != nil && inj.w != nil {
		return true
	}
	cmd := exec.Command("osascript", "-l", "JavaScript", "-e", jxaInputScript)
	pipe, err := cmd.StdinPipe()
	if err != nil {
		return false
	}
	if err := cmd.Start(); err != nil {
		_ = pipe.Close()
		return false
	}
	inj.cmd = cmd
	inj.stdin = pipe
	inj.w = bufio.NewWriter(pipe)
	return true
}

func (inj *InputInjector) stopWorker() {
	inj.mu.Lock()
	defer inj.mu.Unlock()
	if inj.stdin != nil {
		_ = inj.stdin.Close()
		inj.stdin = nil
	}
	if inj.cmd != nil {
		_ = inj.cmd.Process.Kill()
		_ = inj.cmd.Wait()
		inj.cmd = nil
	}
	inj.w = nil
}

func (inj *InputInjector) sendLineLocked(line string) {
	if !inj.ensureWorkerLocked() {
		return
	}
	if _, err := inj.w.WriteString(line + "\n"); err != nil || inj.w.Flush() != nil {
		_ = inj.stdin.Close()
		_ = inj.cmd.Process.Kill()
		_ = inj.cmd.Wait()
		inj.cmd = nil
		inj.stdin = nil
		inj.w = nil
	}
}

// HandleMouse injects a normalized mouse event mapped to the macOS display.
func (inj *InputInjector) HandleMouse(ev protocol.MouseEvent, _ int) {
	if !inj.enabled.Load() {
		return
	}
	inj.mu.Lock()
	defer inj.mu.Unlock()

	x := int((float64(ev.X)/65535.0)*float64(max(1, inj.screenW-1)) + 0.5)
	y := int((float64(ev.Y)/65535.0)*float64(max(1, inj.screenH-1)) + 0.5)

	switch ev.Action {
	case protocol.MouseMove:
		inj.sendLineLocked(fmt.Sprintf("M %d %d", x, y))
	case protocol.MouseDown:
		inj.sendLineLocked(fmt.Sprintf("D %d %d %d", ev.Button, x, y))
	case protocol.MouseUp:
		inj.sendLineLocked(fmt.Sprintf("U %d %d %d", ev.Button, x, y))
	case protocol.MouseWheel:
		steps := int(ev.Delta) / 40
		if steps == 0 {
			if ev.Delta > 0 {
				steps = 1
			} else if ev.Delta < 0 {
				steps = -1
			}
		}
		inj.sendLineLocked(fmt.Sprintf("W %d", steps))
	}
}

// HandleKey injects a keyboard event via macOS CGEvent.
func (inj *InputInjector) HandleKey(ev protocol.KeyEvent) {
	if !inj.enabled.Load() {
		return
	}
	kc, ok := VKToMacKeycode(ev.VK)
	if !ok {
		return
	}
	inj.mu.Lock()
	defer inj.mu.Unlock()

	if ev.Action == protocol.KeyDown {
		inj.held[kc] = true
		inj.sendLineLocked(fmt.Sprintf("K %d 1", kc))
	} else {
		delete(inj.held, kc)
		inj.sendLineLocked(fmt.Sprintf("K %d 0", kc))
	}
}

// SendSystemAction triggers a supported macOS system shortcut (Cmd+Opt+Esc for Force Quit on CAD).
func (inj *InputInjector) SendSystemAction(action string) {
	if !inj.enabled.Load() || action != protocol.ActionCtrlAltDel {
		return
	}
	inj.mu.Lock()
	defer inj.mu.Unlock()
	inj.sendLineLocked("K 55 1")
	inj.sendLineLocked("K 58 1")
	inj.sendLineLocked("K 53 1")
	inj.sendLineLocked("K 53 0")
	inj.sendLineLocked("K 58 0")
	inj.sendLineLocked("K 55 0")
}

// ReleaseAllKeys releases any keys currently held down.
func (inj *InputInjector) ReleaseAllKeys() {
	inj.mu.Lock()
	defer inj.mu.Unlock()
	for kc := range inj.held {
		inj.sendLineLocked(fmt.Sprintf("K %d 0", kc))
	}
	clear(inj.held)
}
