use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;

use tracing::{debug, warn};
use windows::core::{w, PCWSTR};
use windows::Win32::Foundation::{HWND, LPARAM, LRESULT, POINT, WPARAM};
use windows::Win32::System::LibraryLoader::GetModuleHandleW;
use windows::Win32::UI::Shell::{
    Shell_NotifyIconW, NIF_ICON, NIF_MESSAGE, NIF_TIP, NIM_ADD, NIM_DELETE, NIM_MODIFY,
    NOTIFYICONDATAW,
};
use windows::Win32::UI::WindowsAndMessaging::{
    AppendMenuW, CreatePopupMenu, CreateWindowExW, DefWindowProcW, DestroyMenu, DestroyWindow,
    DispatchMessageW, GetCursorPos, GetMessageW, LoadIconW, PostQuitMessage, RegisterClassW,
    SetForegroundWindow, TrackPopupMenu, TranslateMessage, IDI_APPLICATION, MF_SEPARATOR,
    MF_STRING, MSG, TPM_BOTTOMALIGN, TPM_LEFTALIGN, WM_APP, WM_COMMAND, WM_DESTROY, WM_RBUTTONUP,
    WNDCLASSW,
};

const WM_TRAY_ICON: u32 = WM_APP + 1;
const CMD_EXIT: usize = 1001;

/// Handle to update the tray tooltip or shut down the tray thread cleanly.
#[derive(Clone)]
pub struct TrayController {
    shutdown: Arc<AtomicBool>,
    hwnd_raw: Arc<std::sync::atomic::AtomicIsize>,
}

impl TrayController {
    pub fn update_status(&self, tooltip: &str) {
        let raw = self.hwnd_raw.load(Ordering::Acquire);
        if raw == 0 {
            return;
        }
        let hwnd = HWND(raw as *mut _);
        unsafe {
            let mut nid = NOTIFYICONDATAW {
                cbSize: std::mem::size_of::<NOTIFYICONDATAW>() as u32,
                hWnd: hwnd,
                uID: 1,
                uFlags: NIF_TIP,
                ..Default::default()
            };
            write_wide_tip(&mut nid.szTip, tooltip);
            let _ = Shell_NotifyIconW(NIM_MODIFY, &nid);
        }
    }

    pub fn stop(&self) {
        self.shutdown.store(true, Ordering::Release);
        let raw = self.hwnd_raw.load(Ordering::Acquire);
        if raw != 0 {
            unsafe {
                let _ = windows::Win32::UI::WindowsAndMessaging::PostMessageW(
                    HWND(raw as *mut _),
                    WM_DESTROY,
                    WPARAM(0),
                    LPARAM(0),
                );
            }
        }
    }
}

static GLOBAL_EXIT_FLAG: std::sync::OnceLock<Arc<AtomicBool>> = std::sync::OnceLock::new();

/// Spawn a lightweight native Windows system tray icon so the remote user always sees
/// when the agent is running and can click "Desconectar y Salir" at any time.
pub fn spawn_tray_icon(initial_tooltip: String, shutdown_flag: Arc<AtomicBool>) -> TrayController {
    let _ = GLOBAL_EXIT_FLAG.set(shutdown_flag.clone());
    let hwnd_raw = Arc::new(std::sync::atomic::AtomicIsize::new(0));
    let hwnd_raw_clone = hwnd_raw.clone();

    std::thread::Builder::new()
        .name("rmm-tray".into())
        .spawn(move || {
            if let Err(e) = run_tray_loop(&initial_tooltip, hwnd_raw_clone) {
                warn!("Tray icon loop ended: {e}");
            }
        })
        .ok();

    TrayController {
        shutdown: shutdown_flag,
        hwnd_raw,
    }
}

fn run_tray_loop(
    initial_tooltip: &str,
    hwnd_raw: Arc<std::sync::atomic::AtomicIsize>,
) -> anyhow::Result<()> {
    unsafe {
        let hinstance = GetModuleHandleW(None)?;
        let class_name = w!("GeckoAgentTrayWindow");

        let wc = WNDCLASSW {
            lpfnWndProc: Some(tray_wnd_proc),
            hInstance: hinstance.into(),
            lpszClassName: class_name,
            ..Default::default()
        };
        RegisterClassW(&wc);

        let hwnd = CreateWindowExW(
            Default::default(),
            class_name,
            w!("Gecko Remote Agent"),
            Default::default(),
            0,
            0,
            0,
            0,
            None,
            None,
            hinstance,
            None,
        )?;

        hwnd_raw.store(hwnd.0 as isize, Ordering::Release);

        let icon = LoadIconW(None, IDI_APPLICATION).unwrap_or_default();
        let mut nid = NOTIFYICONDATAW {
            cbSize: std::mem::size_of::<NOTIFYICONDATAW>() as u32,
            hWnd: hwnd,
            uID: 1,
            uFlags: NIF_MESSAGE | NIF_ICON | NIF_TIP,
            uCallbackMessage: WM_TRAY_ICON,
            hIcon: icon,
            ..Default::default()
        };
        write_wide_tip(&mut nid.szTip, initial_tooltip);
        let _ = Shell_NotifyIconW(NIM_ADD, &nid);

        debug!("Native system tray icon active");

        let mut msg = MSG::default();
        while GetMessageW(&mut msg, None, 0, 0).as_bool() {
            let _ = TranslateMessage(&msg);
            DispatchMessageW(&msg);
        }

        let _ = Shell_NotifyIconW(NIM_DELETE, &nid);
        hwnd_raw.store(0, Ordering::Release);
    }
    Ok(())
}

unsafe extern "system" fn tray_wnd_proc(
    hwnd: HWND,
    msg: u32,
    wparam: WPARAM,
    lparam: LPARAM,
) -> LRESULT {
    match msg {
        WM_TRAY_ICON => {
            if lparam.0 as u32 == WM_RBUTTONUP {
                if let Ok(menu) = CreatePopupMenu() {
                    let _ = AppendMenuW(
                        menu,
                        MF_STRING,
                        0,
                        PCWSTR(to_wide("Gecko Agente (Activo)").as_ptr()),
                    );
                    let _ = AppendMenuW(menu, MF_SEPARATOR, 0, PCWSTR::null());
                    let _ = AppendMenuW(
                        menu,
                        MF_STRING,
                        CMD_EXIT,
                        PCWSTR(to_wide("Desconectar y Salir").as_ptr()),
                    );
                    let mut pt = POINT::default();
                    let _ = GetCursorPos(&mut pt);
                    let _ = SetForegroundWindow(hwnd);
                    let _ = TrackPopupMenu(menu, TPM_BOTTOMALIGN | TPM_LEFTALIGN, pt.x, pt.y, 0, hwnd, None);
                    let _ = DestroyMenu(menu);
                }
            }
            LRESULT(0)
        }
        WM_COMMAND => {
            if (wparam.0 & 0xFFFF) == CMD_EXIT {
                if let Some(flag) = GLOBAL_EXIT_FLAG.get() {
                    flag.store(true, Ordering::Release);
                }
                let _ = DestroyWindow(hwnd);
            }
            LRESULT(0)
        }
        WM_DESTROY => {
            PostQuitMessage(0);
            LRESULT(0)
        }
        _ => DefWindowProcW(hwnd, msg, wparam, lparam),
    }
}

fn to_wide(s: &str) -> Vec<u16> {
    s.encode_utf16().chain(std::iter::once(0)).collect()
}

fn write_wide_tip(dst: &mut [u16; 128], text: &str) {
    dst.fill(0);
    for (i, ch) in text.encode_utf16().take(127).enumerate() {
        dst[i] = ch;
    }
}
