use std::net::SocketAddr;
use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{anyhow, Result};
use chrono::Utc;
use crossbeam_channel::bounded;
use parking_lot::Mutex;
use quinn::{Connection, Endpoint};
use rmm_decode_win::MfVideoDecoder;
use rmm_net::{
    build_server_config, read_framed, run_lan_discovery_responder, write_framed, FrameReassembler,
};
use rmm_proto::{
    AdminControlMsg, AgentControlMsg, CriticalInputEvent, MouseMoveDatagram, SignedAgentConfig,
    VideoCodec, DATAGRAM_VIDEO_FRAG, PROTOCOL_VERSION,
};
use tokio::sync::mpsc;
use tracing::{error, info, warn};
use uuid::Uuid;

use crate::state::{
    AdminState, AgentSummary, ConnectedAgent, EncodedVideoPacket, TelemetrySnapshot,
};

/// Start both the embedded reverse-connection QUIC server and the signed LAN discovery responder.
pub async fn run_admin_servers(state: Arc<AdminState>) -> Result<()> {
    let disc_cert = state.identity.cert_sha256;
    let quic_port = state.listen_port;
    let disc_port = state.discovery_port;

    tokio::spawn(async move {
        if let Err(e) = run_lan_discovery_responder(disc_cert, quic_port, disc_port).await {
            warn!("LAN Auto-Discovery responder stopped: {e}");
        }
    });

    let server_config = build_server_config(&state.identity)?;
    let bind_addr: SocketAddr = format!("0.0.0.0:{}", state.listen_port).parse()?;
    let endpoint = Endpoint::server(server_config, bind_addr)?;

    info!(%bind_addr, "Gecko embedded QUIC server listening");

    while let Some(incoming) = endpoint.accept().await {
        let state_clone = state.clone();
        tokio::spawn(async move {
            match incoming.await {
                Ok(conn) => {
                    let remote = conn.remote_address();
                    info!(%remote, "Incoming reverse QUIC connection");
                    if let Err(e) = handle_agent_connection(conn, state_clone).await {
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

async fn handle_agent_connection(conn: Connection, state: Arc<AdminState>) -> Result<()> {
    let remote_addr = conn.remote_address().to_string();

    let (mut ctrl_send, mut ctrl_recv) = conn.accept_bi().await?;
    let hello: AgentControlMsg = read_framed(&mut ctrl_recv).await?;

    let (
        machine_id_str,
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
            machine_id,
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
                hex::encode(machine_id),
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
    let video_tx: Arc<Mutex<Option<crossbeam_channel::Sender<EncodedVideoPacket>>>> =
        Arc::new(Mutex::new(None));

    let is_same_pc = machine_id_str == state.local_machine_id;

    let summary = AgentSummary {
        machine_id: machine_id_str.clone(),
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
        is_same_pc,
    };

    state.record_known_device(&summary);

    // Deduplicate: if this exact machine_id already had an older stale socket, close it cleanly!
    {
        let mut map = state.agents.write();
        if let Some(old) = map.insert(
            machine_id_str.clone(),
            ConnectedAgent {
                summary,
                conn: conn.clone(),
                ctrl_tx: ctrl_tx.clone(),
                input_tx,
                video_tx: video_tx.clone(),
            },
        ) {
            info!(machine_id = %machine_id_str, "Replacing older connection from same machine");
            old.conn.close(0u32.into(), b"superseded");
        }
    }
    state.request_repaint();
    info!(machine_id = %machine_id_str, %hostname, %os_user, is_same_pc, "Agent Online");

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

    // 3. Incoming Unreliable Datagram Reader (Video Frames -> Native Decoder)
    let conn_dgram = conn.clone();
    let vtx_dgram = video_tx.clone();
    let ctrl_tx_dgram = ctrl_tx.clone();
    let dgram_reader = tokio::spawn(async move {
        let mut reassembler = FrameReassembler::new();
        while let Ok(raw) = conn_dgram.read_datagram().await {
            if raw.first().copied() == Some(DATAGRAM_VIDEO_FRAG) {
                let res = reassembler.push_datagram(&raw);
                if res.request_idr {
                    let _ = ctrl_tx_dgram.try_send(AdminControlMsg::RequestIdr);
                }
                if let Some(frame) = res.frame {
                    let guard = vtx_dgram.lock();
                    if let Some(ref sender) = *guard {
                        let codec = if frame.is_hevc {
                            VideoCodec::Hevc
                        } else {
                            VideoCodec::H264
                        };
                        let _ = sender.try_send(EncodedVideoPacket {
                            data: frame.data,
                            is_keyframe: frame.is_keyframe,
                            codec,
                        });
                    }
                }
            }
        }
    });

    // 4. Periodic RTT & Heartbeat monitor
    let state_hb = state.clone();
    let mid_hb = machine_id_str.clone();
    let aid_hb = agent_id_str.clone();
    let conn_hb = conn.clone();
    let ctrl_tx_hb = ctrl_tx.clone();
    let heartbeat_task = tokio::spawn(async move {
        let mut nonce = 0u64;
        let mut interval = tokio::time::interval(Duration::from_secs(2));
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
            if let Some(ag) = state_hb.agents.write().get_mut(&mid_hb) {
                if ag.summary.agent_id == aid_hb {
                    ag.summary.rtt_ms = rtt;
                }
            }
            state_hb.request_repaint();
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
                if let Some(ag) = state.agents.write().get_mut(&machine_id_str) {
                    if ag.summary.agent_id == agent_id_str {
                        ag.summary.streaming = true;
                        ag.summary.active_display = display_index;
                        ag.summary.stream_width = width;
                        ag.summary.stream_height = height;
                        ag.summary.encoder_name = Some(encoder_name);
                        ag.summary.is_hardware_encoder = is_hardware;
                    }
                }
                state.request_repaint();
            }
            AgentControlMsg::StreamStopped { .. } => {
                if let Some(ag) = state.agents.write().get_mut(&machine_id_str) {
                    if ag.summary.agent_id == agent_id_str {
                        ag.summary.streaming = false;
                    }
                }
                state.request_repaint();
            }
            AgentControlMsg::Telemetry {
                fps,
                encode_ms,
                capture_ms,
                bitrate_kbps,
                dropped_frames,
            } => {
                let rtt = conn.rtt().as_millis() as u32;
                let dec_ms = state.viewer_frame.decode_ms_x10.load(Ordering::Relaxed) as f32 / 10.0;
                if let Some(ag) = state.agents.write().get_mut(&machine_id_str) {
                    if ag.summary.agent_id == agent_id_str {
                        ag.summary.rtt_ms = rtt;
                        ag.summary.telemetry = TelemetrySnapshot {
                            fps,
                            capture_ms,
                            encode_ms,
                            decode_ms: dec_ms,
                            bitrate_kbps,
                            dropped_frames,
                        };
                    }
                }
                state.request_repaint();
            }
            AgentControlMsg::UserDisconnected => {
                info!(machine_id = %machine_id_str, "Remote user clicked Disconnect");
                break;
            }
            AgentControlMsg::CursorShape { .. }
            | AgentControlMsg::HeartbeatAck { .. }
            | AgentControlMsg::Hello { .. } => {}
        }
    }

    // Cleanup ONLY if this connection is still the active one for `machine_id_str`
    heartbeat_task.abort();
    dgram_reader.abort();
    input_writer.abort();
    ctrl_writer.abort();
    {
        let mut map = state.agents.write();
        if let Some(cur) = map.get(&machine_id_str) {
            if cur.summary.agent_id == agent_id_str {
                let summary_snap = cur.summary.clone();
                map.remove(&machine_id_str);
                drop(map);
                state.record_known_device(&summary_snap);
            }
        }
    }
    state.request_repaint();
    info!(machine_id = %machine_id_str, "Agent offline");
    Ok(())
}

/// Start native video decoding and send `StartStream` to the remote agent.
pub fn start_remote_session(
    state: &Arc<AdminState>,
    machine_id: &str,
    display_index: u32,
    target_fps: u32,
    bitrate_kbps: u32,
    codec: VideoCodec,
) -> Result<()> {
    let (tx, rx) = bounded::<EncodedVideoPacket>(4);

    // Clear previous decoded frame
    *state.viewer_frame.latest.lock() = None;
    state.viewer_frame.version.fetch_add(1, Ordering::AcqRel);

    let ctrl_tx = {
        let agents = state.agents.read();
        let ag = agents
            .get(machine_id)
            .ok_or_else(|| anyhow!("Equipo desconectado"))?;
        *ag.video_tx.lock() = Some(tx);
        ag.ctrl_tx.clone()
    };

    // Spawn dedicated native Media Foundation video decoder thread
    let state_dec = state.clone();
    let ctrl_dec = ctrl_tx.clone();
    std::thread::Builder::new()
        .name("gecko-native-decoder".into())
        .spawn(move || {
            let mut decoder = match MfVideoDecoder::new(codec) {
                Ok(d) => d,
                Err(e) => {
                    error!("Failed to create native MfVideoDecoder: {e:?}");
                    return;
                }
            };
            let mut seen_keyframe = false;

            while let Ok(pkt) = rx.recv() {
                if pkt.codec != decoder.codec() {
                    if let Ok(new_dec) = MfVideoDecoder::new(pkt.codec) {
                        decoder = new_dec;
                        seen_keyframe = false;
                    }
                }
                if !seen_keyframe {
                    if !pkt.is_keyframe {
                        continue;
                    }
                    seen_keyframe = true;
                }

                match decoder.decode(&pkt.data) {
                    Ok(frames) => {
                        for frame in frames {
                            state_dec
                                .viewer_frame
                                .decode_ms_x10
                                .store((frame.decode_ms * 10.0) as u64, Ordering::Relaxed);
                            *state_dec.viewer_frame.latest.lock() = Some(frame);
                            state_dec.viewer_frame.version.fetch_add(1, Ordering::Release);
                            state_dec
                                .viewer_frame
                                .rendered_frames
                                .fetch_add(1, Ordering::Relaxed);
                            state_dec.request_repaint();
                        }
                    }
                    Err(e) => {
                        warn!("Native decode error, requesting IDR: {e:?}");
                        decoder.flush();
                        seen_keyframe = false;
                        let _ = ctrl_dec.try_send(AdminControlMsg::RequestIdr);
                    }
                }
            }
            info!("Native video decoder thread exited cleanly");
        })?;

    let _ = ctrl_tx.try_send(AdminControlMsg::StartStream {
        display_index,
        target_fps: target_fps.clamp(15, 120),
        bitrate_kbps: bitrate_kbps.clamp(1000, 50_000),
        codec,
        max_width: 0,
    });

    Ok(())
}

pub fn stop_remote_session(state: &AdminState, machine_id: &str) {
    let agents = state.agents.read();
    if let Some(ag) = agents.get(machine_id) {
        *ag.video_tx.lock() = None;
        let _ = ag.input_tx.try_send(CriticalInputEvent::ReleaseAll);
        let _ = ag.ctrl_tx.try_send(AdminControlMsg::StopStream);
    }
    *state.viewer_frame.latest.lock() = None;
    state.viewer_frame.version.fetch_add(1, Ordering::AcqRel);
}

pub fn send_mouse_move(
    state: &AdminState,
    machine_id: &str,
    display_index: u8,
    epoch: u8,
    x: u16,
    y: u16,
    seq: u16,
) {
    let agents = state.agents.read();
    if let Some(ag) = agents.get(machine_id) {
        let dgram = MouseMoveDatagram {
            display_index,
            epoch,
            x,
            y,
            seq,
        };
        let _ = ag.conn.send_datagram(dgram.encode().to_vec().into());
    }
}

pub fn send_critical_input(state: &AdminState, machine_id: &str, event: CriticalInputEvent) {
    let agents = state.agents.read();
    if let Some(ag) = agents.get(machine_id) {
        let _ = ag.input_tx.try_send(event);
    }
}

pub fn request_idr(state: &AdminState, machine_id: &str) {
    let agents = state.agents.read();
    if let Some(ag) = agents.get(machine_id) {
        let _ = ag.ctrl_tx.try_send(AdminControlMsg::RequestIdr);
    }
}

pub fn adjust_encoder(state: &AdminState, machine_id: &str, bitrate_kbps: u32, target_fps: u32) {
    let agents = state.agents.read();
    if let Some(ag) = agents.get(machine_id) {
        let _ = ag.ctrl_tx.try_send(AdminControlMsg::AdjustEncoder {
            bitrate_kbps,
            target_fps,
        });
    }
}
