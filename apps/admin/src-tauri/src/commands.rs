use std::process::Command;
use std::sync::Arc;

use rmm_proto::{AdminControlMsg, CriticalInputEvent, MouseButton, MouseMoveDatagram, VideoCodec};
use serde::{Deserialize, Serialize};
use tauri::ipc::{Channel, InvokeResponseBody};
use tauri::{AppHandle, Emitter, Manager, State};

use crate::generator::{
    detect_local_ipv4, generate_configured_client, get_stub_status, GenerateClientRequest,
    GenerateClientResponse, StubStatus,
};
use crate::state::{AdminState, AgentSummary, IssuedTokenRecord};

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ServerInfoResponse {
    pub listen_port: u16,
    pub local_ips: Vec<String>,
    pub cert_fingerprint: String,
    pub stub_status: StubStatus,
    pub generated_dir: String,
    pub issued_tokens: Vec<IssuedTokenRecord>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct StartSessionRequest {
    pub agent_id: String,
    pub display_index: u32,
    pub target_fps: u32,
    pub bitrate_kbps: u32,
    pub codec: String,
    pub max_width: u32,
}

#[derive(Debug, Deserialize)]
#[serde(tag = "kind", rename_all = "camelCase")]
pub enum FrontendCriticalInput {
    Key {
        scancode: u16,
        extended: bool,
        pressed: bool,
    },
    MouseButton {
        button: String,
        pressed: bool,
        x: u16,
        y: u16,
    },
    MouseWheel {
        delta_x: i16,
        delta_y: i16,
    },
    ReleaseAll,
}

#[tauri::command]
pub fn get_server_info(
    app: AppHandle,
    state: State<'_, Arc<AdminState>>,
) -> Result<ServerInfoResponse, String> {
    let res_dir = app.path().resource_dir().ok();
    Ok(ServerInfoResponse {
        listen_port: state.listen_port,
        local_ips: detect_local_ipv4(),
        cert_fingerprint: state.identity.cert_fingerprint_hex(),
        stub_status: get_stub_status(res_dir.as_deref()),
        generated_dir: state.generated_clients_dir().to_string_lossy().to_string(),
        issued_tokens: state.tokens.read().clone(),
    })
}

#[tauri::command]
pub fn list_agents(state: State<'_, Arc<AdminState>>) -> Vec<AgentSummary> {
    state.agent_summaries()
}

#[tauri::command]
pub fn generate_client(
    app: AppHandle,
    state: State<'_, Arc<AdminState>>,
    req: GenerateClientRequest,
) -> Result<GenerateClientResponse, String> {
    let res_dir = app.path().resource_dir().ok();
    generate_configured_client(&state, res_dir.as_deref(), req).map_err(|e| e.to_string())
}

#[tauri::command]
pub fn revoke_token(
    app: AppHandle,
    state: State<'_, Arc<AdminState>>,
    token_id: String,
) -> Result<(), String> {
    let parsed = uuid::Uuid::parse_str(&token_id).map_err(|e| e.to_string())?;
    state.revoked_tokens.write().insert(*parsed.as_bytes());
    for rec in state.tokens.write().iter_mut() {
        if rec.token_id == token_id {
            rec.revoked = true;
        }
    }
    state.persist_tokens();

    // Disconnect any active agent currently authenticated with this token
    let agents = state.agents.read();
    for ag in agents.values() {
        if ag.summary.token_id == token_id {
            let _ = ag.ctrl_tx.try_send(AdminControlMsg::Disconnect);
            ag.conn.close(1u32.into(), b"token revoked");
        }
    }
    let _ = app.emit("agents-updated", state.agent_summaries());
    Ok(())
}

#[tauri::command]
pub fn reveal_in_explorer(path: String) -> Result<(), String> {
    let p = std::path::Path::new(&path);
    if p.is_file() {
        Command::new("explorer.exe")
            .args(["/select,", &path])
            .spawn()
            .map_err(|e| e.to_string())?;
    } else {
        Command::new("explorer.exe")
            .arg(&path)
            .spawn()
            .map_err(|e| e.to_string())?;
    }
    Ok(())
}

#[tauri::command]
pub async fn start_remote_session(
    state: State<'_, Arc<AdminState>>,
    req: StartSessionRequest,
    video_channel: Channel<InvokeResponseBody>,
) -> Result<(), String> {
    let codec = match req.codec.to_lowercase().as_str() {
        "hevc" | "h265" => VideoCodec::Hevc,
        _ => VideoCodec::H264,
    };

    let ctrl_tx = {
        let agents = state.agents.read();
        let ag = agents
            .get(&req.agent_id)
            .ok_or_else(|| "Agente no encontrado o desconectado".to_string())?;
        *ag.video_channel.lock() = Some(video_channel);
        ag.ctrl_tx.clone()
    };

    ctrl_tx
        .send(AdminControlMsg::StartStream {
            display_index: req.display_index,
            target_fps: req.target_fps.clamp(15, 120),
            bitrate_kbps: req.bitrate_kbps.clamp(1000, 50_000),
            codec,
            max_width: req.max_width,
        })
        .await
        .map_err(|e| e.to_string())?;

    Ok(())
}

#[tauri::command]
pub async fn stop_remote_session(
    state: State<'_, Arc<AdminState>>,
    agent_id: String,
) -> Result<(), String> {
    let ctrl_tx = {
        let agents = state.agents.read();
        if let Some(ag) = agents.get(&agent_id) {
            *ag.video_channel.lock() = None;
            Some(ag.ctrl_tx.clone())
        } else {
            None
        }
    };

    if let Some(tx) = ctrl_tx {
        let _ = tx.send(AdminControlMsg::StopStream).await;
    }
    Ok(())
}

#[tauri::command]
pub fn send_mouse_move(
    state: State<'_, Arc<AdminState>>,
    agent_id: String,
    display_index: u8,
    x: u16,
    y: u16,
    seq: u16,
) -> Result<(), String> {
    let agents = state.agents.read();
    if let Some(ag) = agents.get(&agent_id) {
        let dgram = MouseMoveDatagram {
            display_index,
            x,
            y,
            seq,
        };
        let _ = ag.conn.send_datagram(dgram.encode().to_vec().into());
    }
    Ok(())
}

#[tauri::command]
pub fn send_critical_input(
    state: State<'_, Arc<AdminState>>,
    agent_id: String,
    event: FrontendCriticalInput,
) -> Result<(), String> {
    let proto_ev = match event {
        FrontendCriticalInput::Key {
            scancode,
            extended,
            pressed,
        } => CriticalInputEvent::Key {
            scancode,
            extended,
            pressed,
        },
        FrontendCriticalInput::MouseButton {
            button,
            pressed,
            x,
            y,
        } => {
            let btn = match button.as_str() {
                "right" => MouseButton::Right,
                "middle" => MouseButton::Middle,
                "x1" => MouseButton::X1,
                "x2" => MouseButton::X2,
                _ => MouseButton::Left,
            };
            CriticalInputEvent::MouseButton {
                button: btn,
                pressed,
                x,
                y,
            }
        }
        FrontendCriticalInput::MouseWheel { delta_x, delta_y } => {
            CriticalInputEvent::MouseWheel { delta_x, delta_y }
        }
        FrontendCriticalInput::ReleaseAll => CriticalInputEvent::ReleaseAll,
    };

    let agents = state.agents.read();
    if let Some(ag) = agents.get(&agent_id) {
        let _ = ag.input_tx.try_send(proto_ev);
    }
    Ok(())
}

#[tauri::command]
pub fn request_idr(state: State<'_, Arc<AdminState>>, agent_id: String) -> Result<(), String> {
    let agents = state.agents.read();
    if let Some(ag) = agents.get(&agent_id) {
        let _ = ag.ctrl_tx.try_send(AdminControlMsg::RequestIdr);
    }
    Ok(())
}

#[tauri::command]
pub fn adjust_encoder(
    state: State<'_, Arc<AdminState>>,
    agent_id: String,
    bitrate_kbps: u32,
    target_fps: u32,
) -> Result<(), String> {
    let agents = state.agents.read();
    if let Some(ag) = agents.get(&agent_id) {
        let _ = ag.ctrl_tx.try_send(AdminControlMsg::AdjustEncoder {
            bitrate_kbps,
            target_fps,
        });
    }
    Ok(())
}

#[tauri::command]
pub fn disconnect_agent(state: State<'_, Arc<AdminState>>, agent_id: String) -> Result<(), String> {
    let agents = state.agents.read();
    if let Some(ag) = agents.get(&agent_id) {
        let _ = ag.ctrl_tx.try_send(AdminControlMsg::Disconnect);
        ag.conn.close(0u32.into(), b"admin disconnect");
    }
    Ok(())
}
