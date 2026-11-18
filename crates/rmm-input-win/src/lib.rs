use std::collections::HashSet;
use std::mem::size_of;

use rmm_proto::{CriticalInputEvent, DisplayInfo, MouseButton, MouseMoveDatagram};
use windows::Win32::UI::Input::KeyboardAndMouse::{
    SendInput, INPUT, INPUT_0, INPUT_KEYBOARD, INPUT_MOUSE, KEYBDINPUT, KEYBD_EVENT_FLAGS,
    KEYEVENTF_EXTENDEDKEY, KEYEVENTF_KEYUP, KEYEVENTF_SCANCODE, MOUSEEVENTF_ABSOLUTE,
    MOUSEEVENTF_HWHEEL, MOUSEEVENTF_LEFTDOWN, MOUSEEVENTF_LEFTUP, MOUSEEVENTF_MIDDLEDOWN,
    MOUSEEVENTF_MIDDLEUP, MOUSEEVENTF_MOVE, MOUSEEVENTF_RIGHTDOWN, MOUSEEVENTF_RIGHTUP,
    MOUSEEVENTF_VIRTUALDESK, MOUSEEVENTF_WHEEL, MOUSEEVENTF_XDOWN, MOUSEEVENTF_XUP, MOUSEINPUT,
    MOUSE_EVENT_FLAGS, VIRTUAL_KEY,
};
use windows::Win32::UI::WindowsAndMessaging::{
    GetSystemMetrics, SM_CXVIRTUALSCREEN, SM_CYVIRTUALSCREEN, SM_XVIRTUALSCREEN, SM_YVIRTUALSCREEN,
};

const XBUTTON1: u32 = 0x0001;
const XBUTTON2: u32 = 0x0002;

/// High-precision, layout-independent Windows input injector with automatic stuck-key protection.
pub struct WinInputInjector {
    held_keys: HashSet<(u16, bool)>,
    held_buttons: HashSet<MouseButton>,
    last_move_seq: Option<u16>,
    active_display: Option<DisplayInfo>,
}

impl Default for WinInputInjector {
    fn default() -> Self {
        Self::new()
    }
}

impl WinInputInjector {
    pub fn new() -> Self {
        Self {
            held_keys: HashSet::new(),
            held_buttons: HashSet::new(),
            last_move_seq: None,
            active_display: None,
        }
    }

    /// Update the active target display geometry so normalized coordinates (0..65535)
    /// map accurately onto the selected monitor within the Windows virtual desktop.
    pub fn set_active_display(&mut self, display: Option<DisplayInfo>) {
        self.active_display = display;
        self.last_move_seq = None;
    }

    /// Inject an unreliable mouse-move datagram, discarding stale out-of-order packets.
    pub fn handle_mouse_move(&mut self, msg: MouseMoveDatagram) {
        if let Some(prev) = self.last_move_seq {
            let diff = msg.seq.wrapping_sub(prev);
            if diff == 0 || diff > 32_768 {
                return;
            }
        }
        self.last_move_seq = Some(msg.seq);

        let (vx, vy) = self.map_to_virtual_desktop(msg.x, msg.y);
        send_mouse_input(
            vx,
            vy,
            0,
            MOUSEEVENTF_MOVE | MOUSEEVENTF_ABSOLUTE | MOUSEEVENTF_VIRTUALDESK,
        );
    }

    /// Inject a reliable critical input event (keyboard scan code, mouse button, wheel, release-all).
    pub fn handle_critical_event(&mut self, event: CriticalInputEvent) {
        match event {
            CriticalInputEvent::Key {
                scancode,
                extended,
                pressed,
            } => {
                if pressed {
                    self.held_keys.insert((scancode, extended));
                } else {
                    self.held_keys.remove(&(scancode, extended));
                }
                send_key_scancode(scancode, extended, pressed);
            }
            CriticalInputEvent::MouseButton {
                button,
                pressed,
                x,
                y,
            } => {
                if pressed {
                    self.held_buttons.insert(button);
                } else {
                    self.held_buttons.remove(&button);
                }
                let (vx, vy) = self.map_to_virtual_desktop(x, y);
                let (flag, data) = button_to_win32(button, pressed);
                send_mouse_input(
                    vx,
                    vy,
                    data,
                    flag | MOUSEEVENTF_MOVE | MOUSEEVENTF_ABSOLUTE | MOUSEEVENTF_VIRTUALDESK,
                );
            }
            CriticalInputEvent::MouseWheel { delta_x, delta_y } => {
                if delta_y != 0 {
                    send_mouse_input(0, 0, delta_y as i32 as u32, MOUSEEVENTF_WHEEL);
                }
                if delta_x != 0 {
                    send_mouse_input(0, 0, delta_x as i32 as u32, MOUSEEVENTF_HWHEEL);
                }
            }
            CriticalInputEvent::ReleaseAll => {
                self.release_all();
            }
        }
    }

    /// Synthesize KeyUp / MouseUp for all currently held inputs (called on disconnect or focus loss).
    pub fn release_all(&mut self) {
        for (scancode, extended) in self.held_keys.drain() {
            send_key_scancode(scancode, extended, false);
        }
        for button in self.held_buttons.drain() {
            let (flag, data) = button_to_win32(button, false);
            send_mouse_input(0, 0, data, flag);
        }
    }

    /// Maps normalized monitor coordinates (0..65535) to Windows Virtual Desktop coordinates (0..65535).
    fn map_to_virtual_desktop(&self, norm_x: u16, norm_y: u16) -> (i32, i32) {
        let Some(ref disp) = self.active_display else {
            return (norm_x as i32, norm_y as i32);
        };

        let (v_left, v_top, v_w, v_h) = unsafe {
            (
                GetSystemMetrics(SM_XVIRTUALSCREEN),
                GetSystemMetrics(SM_YVIRTUALSCREEN),
                GetSystemMetrics(SM_CXVIRTUALSCREEN).max(1),
                GetSystemMetrics(SM_CYVIRTUALSCREEN).max(1),
            )
        };

        let px = disp.left as f64 + (norm_x as f64 / 65535.0) * (disp.width.saturating_sub(1) as f64);
        let py = disp.top as f64 + (norm_y as f64 / 65535.0) * (disp.height.saturating_sub(1) as f64);

        let vx = (((px - v_left as f64) * 65535.0) / (v_w.saturating_sub(1).max(1) as f64))
            .round()
            .clamp(0.0, 65535.0) as i32;
        let vy = (((py - v_top as f64) * 65535.0) / (v_h.saturating_sub(1).max(1) as f64))
            .round()
            .clamp(0.0, 65535.0) as i32;

        (vx, vy)
    }
}

impl Drop for WinInputInjector {
    fn drop(&mut self) {
        self.release_all();
    }
}

fn button_to_win32(button: MouseButton, pressed: bool) -> (MOUSE_EVENT_FLAGS, u32) {
    match (button, pressed) {
        (MouseButton::Left, true) => (MOUSEEVENTF_LEFTDOWN, 0),
        (MouseButton::Left, false) => (MOUSEEVENTF_LEFTUP, 0),
        (MouseButton::Right, true) => (MOUSEEVENTF_RIGHTDOWN, 0),
        (MouseButton::Right, false) => (MOUSEEVENTF_RIGHTUP, 0),
        (MouseButton::Middle, true) => (MOUSEEVENTF_MIDDLEDOWN, 0),
        (MouseButton::Middle, false) => (MOUSEEVENTF_MIDDLEUP, 0),
        (MouseButton::X1, true) => (MOUSEEVENTF_XDOWN, XBUTTON1),
        (MouseButton::X1, false) => (MOUSEEVENTF_XUP, XBUTTON1),
        (MouseButton::X2, true) => (MOUSEEVENTF_XDOWN, XBUTTON2),
        (MouseButton::X2, false) => (MOUSEEVENTF_XUP, XBUTTON2),
    }
}

fn send_mouse_input(dx: i32, dy: i32, mouse_data: u32, flags: MOUSE_EVENT_FLAGS) {
    let input = INPUT {
        r#type: INPUT_MOUSE,
        Anonymous: INPUT_0 {
            mi: MOUSEINPUT {
                dx,
                dy,
                mouseData: mouse_data,
                dwFlags: flags,
                time: 0,
                dwExtraInfo: 0,
            },
        },
    };
    unsafe {
        let _ = SendInput(&[input], size_of::<INPUT>() as i32);
    }
}

fn send_key_scancode(scancode: u16, extended: bool, pressed: bool) {
    let mut flags: KEYBD_EVENT_FLAGS = KEYEVENTF_SCANCODE;
    if extended {
        flags |= KEYEVENTF_EXTENDEDKEY;
    }
    if !pressed {
        flags |= KEYEVENTF_KEYUP;
    }
    let input = INPUT {
        r#type: INPUT_KEYBOARD,
        Anonymous: INPUT_0 {
            ki: KEYBDINPUT {
                wVk: VIRTUAL_KEY(0),
                wScan: scancode,
                dwFlags: flags,
                time: 0,
                dwExtraInfo: 0,
            },
        },
    };
    unsafe {
        let _ = SendInput(&[input], size_of::<INPUT>() as i32);
    }
}
