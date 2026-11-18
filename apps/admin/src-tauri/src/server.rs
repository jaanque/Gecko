use std::net::SocketAddr;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{anyhow, Result};
use base64::engine::general_purpose::STANDARD as B64;
use base64::Engine;
use chrono::Utc;
use parking_lot::Mutex;
use quinn::{Connection, Endpoint};
use rmm_net::{build_server_config, read_framed, write_framed, FrameReassembler};
use rmm_proto::{
    AdminControlMsg, AgentControlMsg, CriticalInputEvent, CursorPosDatagram, SignedAgentConfig,
    DATAGRAM_CURSOR_POS, DATAGRAM_VIDEO_FRAG, PROTOCOL_VERSION, VIDEO_FLAG_HEVC, VIDEO_FLAG_KEYFRAME,
};
use serde::Serialize;
use tauri::ipc::InvokeResponseBody;
use tauri::{AppHandle, Emitter};
use tokio::sync::mpsc;
use tracing::{error, info, warn};
use uuid::Uuid;

use crate::state::{AdminState, AgentSummary, ConnectedAgent, TelemetrySnapshot};

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct CursorShapePayload {
    agent_id: String,
    shape_id: String,
    width: u32,
    height: u32,
    hotspot_x: i32,
    hotspot_y: i32,
    rgba_b64: String,
}

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct CursorPosPayload {
    agent_id: String,
    visible: bool,
    x: u16,
    y: u16,
}

/// Start the embedded reverse-connection QUIC server inside the Admin process.
pub async fn run_quic_server(state: Arc<AdminState>, app: AppHandle) -> Result<()> {
    let server_config = build_server_config(&state.identity)?;
    let bind_addr: SocketAddr = format!("0.0.0.0:{}", state.listen_port).parse()?;
    let endpoint = Endpoint::server(server_config, bind_addr)?;

    info!(%bind_addr, "Embedded Admin QUIC reverse-connection server listening");

    while let Some(incoming) = endpoint.accept().await {
        let state_clone = state.clone();
        let app_clone = app.clone();
        tokio::spawn(async move {
            match incoming.await {
                Ok(conn) => {
                    let remote = conn.remote_address();
                    info!(%remote, "Incoming reverse QUIC connection");
                    if let Err(e) = handle_agent_connection(conn, state_clone, app_clone).await {
                        warn!(%remote, error = %e, "Agent connection ended");
                    }
                }
                Err(e) => {
                    warn!("QUIC handshake failed: {e}");
                }
            }
        });
    }

    Ok(())
}

async fn handle_agent_connection(
    conn: Connection,
    state: Arc<AdminState>,
    app: AppHandle,
) -> Result<()> {
    let remote_addr = conn.remote_address().to_string();

    // Wait for Agent to open bidirectional Control Stream #0
    let (mut ctrl_send, mut ctrl_recv) = conn.accept_bi().await?;
    let hello: AgentControlMsg = read_framed(&mut ctrl_recv).await?;

    let (
        agent_id_str,
        token_id_str,
        label,
        hostname,
        os_user,
        os_version,
        gpu_name,
        displays,
        supported_codecs,
    ) = match hello {
        AgentControlMsg::Hello {
            protocol_version,
            agent_id,
            token_id,
            label,
            hostname,
            os_user,
            os_version,
            gpu_name,
            displays,
            supported_codecs,
            signed_token,
        } => {
            if protocol_version != PROTOCOL_VERSION {
                let _ = write_framed(
                    &mut ctrl_send,
                    &AdminControlMsg::Welcome {
                        accepted: false,
                        reason: Some(format!("Protocol version mismatch ({protocol_version})")),
                        session_id: [0; 16],
                    },
                )
                .await;
                return Err(anyhow!("Protocol version mismatch"));
            }

            // Strictly verify the Ed25519 signature of the agent's enrollment token against THIS Admin's key
            let signed_cfg: SignedAgentConfig = postcard::from_bytes(&signed_token)?;
            let expected_vk = state.identity.verifying_key().to_bytes();
            if signed_cfg.verify(Some(&expected_vk)).is_err() || signed_cfg.data.token_id != token_id
            {
                let _ = write_framed(
                    &mut ctrl_send,
                    &AdminControlMsg::Welcome {
                        accepted: false,
                        reason: Some("Invalid Ed25519 enrollment signature".into()),
                        session_id: [0; 16],
                    },
                )
                .await;
                return Err(anyhow!("Invalid token signature from {remote_addr}"));
            }

            let now_unix = SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap_or_default()
                .as_secs();
            if signed_cfg.check_not_expired(now_unix).is_err() || state.is_token_revoked(&token_id)
            {
                let _ = write_framed(
                    &mut ctrl_send,
                    &AdminControlMsg::Welcome {
                        accepted: false,
                        reason: Some("Token expired or revoked".into()),
                        session_id: [0; 16],
                    },
                )
                .await;
                return Err(anyhow!("Token expired or revoked"));
            }

            (
                Uuid::from_bytes(agent_id).to_string(),
                Uuid::from_bytes(token_id).to_string(),
                label,
                hostname,
                os_user,
                os_version,
                gpu_name,
                displays,
                supported_codecs,
            )
        }
        other => {
            return Err(anyhow!("Expected ClientHello, got {other:?}"));
        }
    };

    let session_id = *Uuid::new_v4().as_bytes();
    write_framed(
        &mut ctrl_send,
        &AdminControlMsg::Welcome {
            accepted: true,
            reason: None,
            session_id,
        },
    )
    .await?;

    let (ctrl_tx, mut ctrl_rx) = mpsc::channel::<AdminControlMsg>(64);
    let (input_tx, mut input_rx) = mpsc::channel::<CriticalInputEvent>(256);
    let video_channel: Arc<Mutex<Option<tauri::ipc::Channel<InvokeResponseBody>>>> =
        Arc::new(Mutex::new(None));

    let summary = AgentSummary {
        agent_id: agent_id_str.clone(),
        token_id: token_id_str,
        label,
        hostname: hostname.clone(),
        os_user: os_user.clone(),
        os_version,
        gpu_name,
        remote_addr,
        displays,
        supported_codecs,
        online_since: Utc::now(),
        rtt_ms: conn.rtt().as_millis() as u32,
        streaming: false,
        active_display: 0,
        stream_width: 0,
        stream_height: 0,
        encoder_name: None,
        is_hardware_encoder: false,
        telemetry: TelemetrySnapshot::default(),
    };

    state.agents.write().insert(
        agent_id_str.clone(),
        ConnectedAgent {
            summary,
            conn: conn.clone(),
            ctrl_tx: ctrl_tx.clone(),
            input_tx,
            video_channel: video_channel.clone(),
        },
    );
    let _ = app.emit("agents-updated", state.agent_summaries());
    info!(agent_id = %agent_id_str, %hostname, %os_user, "Agent registered Online");

    // 1. Outbound Control Stream writer
    let ctrl_writer = tokio::spawn(async move {
        while let Some(msg) = ctrl_rx.recv().await {
            if let Err(e) = write_framed(&mut ctrl_send, &msg).await {
                warn!("Admin control write failed: {e}");
                break;
            }
        }
    });

    // 2. Dedicated High-Priority Reliable Stream for Critical Inputs (Keyboard / Mouse Buttons)
    let conn_input = conn.clone();
    let input_writer = tokio::spawn(async move {
        match conn_input.open_uni().await {
            Ok(mut uni_send) => {
                while let Some(ev) = input_rx.recv().await {
                    if let Err(e) = write_framed(&mut uni_send, &ev).await {
                        warn!("Critical input stream write failed: {e}");
                        break;
                    }
                }
            }
            Err(e) => error!("Failed to open reliable input uni-stream: {e}"),
        }
    });

    // 3. Incoming Unreliable Datagram Reader (Video Frames + Cursor Positions)
    let conn_dgram = conn.clone();
    let vchan_dgram = video_channel.clone();
    let ctrl_tx_dgram = ctrl_tx.clone();
    let app_dgram = app.clone();
    let aid_dgram = agent_id_str.clone();
    let dgram_reader = tokio::spawn(async move {
        let mut reassembler = FrameReassembler::new();
        while let Ok(raw) = conn_dgram.read_datagram().await {
            match raw.first().copied() {
                Some(DATAGRAM_VIDEO_FRAG) => {
                    let res = reassembler.push_datagram(&raw);
                    if res.request_idr {
                        let _ = ctrl_tx_dgram.try_send(AdminControlMsg::RequestIdr);
                    }
                    if let Some(frame) = res.frame {
                        let guard = vchan_dgram.lock();
                        if let Some(ref channel) = *guard {
                            // Pack compact 12-byte header + Annex-B H.264/HEVC payload as raw binary
                            let mut packet = Vec::with_capacity(12 + frame.data.len());
                            let mut flags = 0u8;
                            if frame.is_keyframe {
                                flags |= VIDEO_FLAG_KEYFRAME;
                            }
                            if frame.is_hevc {
                                flags |= VIDEO_FLAG_HEVC;
                            }
                            packet.push(flags);
                            packet.push(0);
                            packet.extend_from_slice(&frame.frame_id.to_le_bytes());
                            packet.extend_from_slice(&frame.capture_ts_us.to_le_bytes());
                            packet.extend_from_slice(&(frame.data.len() as u32).to_le_bytes());
                            packet.extend_from_slice(&frame.data);
                            let _ = channel.send(InvokeResponseBody::Raw(packet));
                        }
                    }
                }
                Some(DATAGRAM_CURSOR_POS) => {
                    if vchan_dgram.lock().is_some() {
                        if let Some(cp) = CursorPosDatagram::decode(&raw) {
                            let _ = app_dgram.emit(
                                "cursor-pos",
                                CursorPosPayload {
                                    agent_id: aid_dgram.clone(),
                                    visible: cp.visible,
                                    x: cp.x,
                                    y: cp.y,
                                },
                            );
                        }
                    }
                }
                _ => {}
            }
        }
    });

    // 4. Periodic RTT & Heartbeat monitor
    let state_hb = state.clone();
    let app_hb = app.clone();
    let aid_hb = agent_id_str.clone();
    let conn_hb = conn.clone();
    let ctrl_tx_hb = ctrl_tx.clone();
    let heartbeat_task = tokio::spawn(async move {
        let mut nonce = 0u64;
        let mut interval = tokio::time::interval(Duration::from_secs(3));
        loop {
            interval.tick().await;
            nonce = nonce.wrapping_add(1);
            if ctrl_tx_hb
                .try_send(AdminControlMsg::Heartbeat { nonce })
                .is_err()
            {
                break;
            }
            let rtt = conn_hb.rtt().as_millis() as u32;
            if let Some(ag) = state_hb.agents.write().get_mut(&aid_hb) {
                ag.summary.rtt_ms = rtt;
            }
            let _ = app_hb.emit("agents-updated", state_hb.agent_summaries());
        }
    });

    // 5. Inbound Control Stream reader
    while let Ok(msg) = read_framed::<AgentControlMsg>(&mut ctrl_recv).await {
        match msg {
            AgentControlMsg::StreamStarted {
                display_index,
                width,
                height,
                encoder_name,
                is_hardware,
                ..
            } => {
                if let Some(ag) = state.agents.write().get_mut(&agent_id_str) {
                    ag.summary.streaming = true;
                    ag.summary.active_display = display_index;
                    ag.summary.stream_width = width;
                    ag.summary.stream_height = height;
                    ag.summary.encoder_name = Some(encoder_name);
                    ag.summary.is_hardware_encoder = is_hardware;
                }
                let _ = app.emit("agents-updated", state.agent_summaries());
            }
            AgentControlMsg::StreamStopped { .. } => {
                if let Some(ag) = state.agents.write().get_mut(&agent_id_str) {
                    ag.summary.streaming = false;
                }
                let _ = app.emit("agents-updated", state.agent_summaries());
            }
            AgentControlMsg::Telemetry {
                fps,
                encode_ms,
                capture_ms,
                bitrate_kbps,
                dropped_frames,
            } => {
                let rtt = conn.rtt().as_millis() as u32;
                if let Some(ag) = state.agents.write().get_mut(&agent_id_str) {
                    ag.summary.rtt_ms = rtt;
                    ag.summary.telemetry = TelemetrySnapshot {
                        fps,
                        capture_ms,
                        encode_ms,
                        bitrate_kbps,
                        dropped_frames,
                    };
                }
                let _ = app.emit("agents-updated", state.agent_summaries());
            }
            AgentControlMsg::CursorShape {
                shape_id,
                width,
                height,
                hotspot_x,
                hotspot_y,
                rgba,
            } => {
                let _ = app.emit(
                    "cursor-shape",
                    CursorShapePayload {
                        agent_id: agent_id_str.clone(),
                        shape_id: shape_id.to_string(),
                        width,
                        height,
                        hotspot_x,
                        hotspot_y,
                        rgba_b64: B64.encode(&rgba),
                    },
                );
            }
            AgentControlMsg::UserDisconnected => {
                info!(agent_id = %agent_id_str, "Remote user clicked Disconnect");
                break;
            }
            AgentControlMsg::HeartbeatAck { .. } | AgentControlMsg::Hello { .. } => {}
        }
    }

    // Cleanup on disconnect
    heartbeat_task.abort();
    dgram_reader.abort();
    input_writer.abort();
    ctrl_writer.abort();
    state.agents.write().remove(&agent_id_str);
    let _ = app.emit("agents-updated", state.agent_summaries());
    info!(agent_id = %agent_id_str, "Agent offline and removed from dashboard");
    Ok(())
}
