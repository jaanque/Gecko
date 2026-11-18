#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

mod tray;

use std::net::SocketAddr;
use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use anyhow::{anyhow, Context, Result};
use crossbeam_channel::{bounded, Receiver, Sender};
use parking_lot::Mutex;
use quinn::Endpoint;
use rmm_capture_win::{enumerate_displays_and_gpu, CaptureFrameResult, DxgiZeroCopyCapturer};
use rmm_encode_win::MfHardwareEncoder;
use rmm_input_win::WinInputInjector;
use rmm_net::{
    build_pinned_client_config, fragment_video_frame, read_framed, write_framed,
    DEFAULT_MAX_DATAGRAM_BYTES,
};
use rmm_proto::{
    read_config_from_current_exe, AdminControlMsg, AgentControlMsg, CriticalInputEvent,
    CursorPosDatagram, MouseMoveDatagram, SignedAgentConfig, VideoCodec,
    DATAGRAM_MOUSE_MOVE, PROTOCOL_VERSION, VIDEO_FLAG_HEVC, VIDEO_FLAG_KEYFRAME,
};
use tokio::sync::mpsc;
use tracing::{error, info, warn};
use uuid::Uuid;
use windows::Win32::System::Threading::{
    GetCurrentThread, SetThreadPriority, THREAD_PRIORITY_ABOVE_NORMAL,
};

struct StreamCommandState {
    stop_flag: Arc<AtomicBool>,
    force_idr: Arc<AtomicBool>,
    target_bitrate_kbps: Arc<AtomicU32>,
    target_fps: Arc<AtomicU32>,
}

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "rmm_agent=info,rmm_net=info,rmm_capture_win=info,rmm_encode_win=info".into()),
        )
        .init();

    // 1. Read Zero-Config signed overlay embedded at the tail of this executable.
    let config = read_config_from_current_exe().context(
        "No valid embedded RMM configuration found. Generate this executable from the Admin Panel.",
    )?;

    let now_unix = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs();
    config.check_not_expired(now_unix)?;

    info!(
        label = %config.data.label,
        admin = %config.data.admin_name,
        endpoints = ?config.data.endpoints,
        "Loaded verified Zero-Config overlay"
    );

    let shutdown = Arc::new(AtomicBool::new(false));
    let tray = tray::spawn_tray_icon(
        format!("RMM Agente — Conectando a {}...", config.data.admin_name),
        shutdown.clone(),
    );

    let agent_id = *Uuid::new_v4().as_bytes();
    let client_cfg = build_pinned_client_config(config.data.admin_cert_sha256)?;

    let mut endpoint = Endpoint::client("0.0.0.0:0".parse::<SocketAddr>()?)?;
    endpoint.set_default_client_config(client_cfg);

    // 2. Resilient Phone-Home loop with exponential backoff.
    let mut backoff_ms = 500u64;
    let mut ep_idx = 0usize;

    while !shutdown.load(Ordering::Relaxed) {
        if config.data.endpoints.is_empty() {
            error!("Configuration contains no Admin endpoints");
            break;
        }
        let target = &config.data.endpoints[ep_idx % config.data.endpoints.len()];
        ep_idx = ep_idx.wrapping_add(1);

        match connect_and_serve(&endpoint, target, agent_id, &config, &shutdown, &tray).await {
            Ok(()) => {
                info!("Session closed cleanly");
                backoff_ms = 500;
            }
            Err(e) => {
                warn!(target = %target, error = %e, "Connection cycle ended");
            }
        }

        if shutdown.load(Ordering::Relaxed) {
            break;
        }

        tray.update_status(&format!(
            "RMM Agente — Reconectando a {}...",
            config.data.admin_name
        ));
        tokio::time::sleep(Duration::from_millis(backoff_ms)).await;
        backoff_ms = (backoff_ms * 2).min(10_000);
    }

    tray.stop();
    endpoint.wait_idle().await;
    Ok(())
}

async fn connect_and_serve(
    endpoint: &Endpoint,
    target_addr: &str,
    agent_id: [u8; 16],
    config: &SignedAgentConfig,
    shutdown: &Arc<AtomicBool>,
    tray: &tray::TrayController,
) -> Result<()> {
    let resolved: SocketAddr = tokio::net::lookup_host(target_addr)
        .await
        .with_context(|| format!("DNS lookup failed for {target_addr}"))?
        .next()
        .ok_or_else(|| anyhow!("No IP resolved for {target_addr}"))?;

    info!(%resolved, "Initiating reverse QUIC connection (Phone Home)");
    let conn = endpoint.connect(resolved, "rmm-admin.local")?.await?;
    info!(%resolved, "QUIC TLS 1.3 tunnel established with pinned certificate");

    // Open bidirectional Control Stream #0
    let (mut ctrl_send, mut ctrl_recv) = conn.open_bi().await?;

    let (gpu_name, displays) =
        enumerate_displays_and_gpu().unwrap_or_else(|_| ("Unknown GPU".into(), Vec::new()));

    let hello = AgentControlMsg::Hello {
        protocol_version: PROTOCOL_VERSION,
        agent_id,
        token_id: config.data.token_id,
        label: config.data.label.clone(),
        hostname: hostname::get()
            .unwrap_or_default()
            .to_string_lossy()
            .to_string(),
        os_user: whoami::username(),
        os_version: format!("Windows ({})", std::env::consts::ARCH),
        gpu_name,
        displays: displays.clone(),
        supported_codecs: vec![VideoCodec::H264, VideoCodec::Hevc],
        signed_token: postcard::to_allocvec(config)?,
    };

    write_framed(&mut ctrl_send, &hello).await?;

    // Wait for Admin Welcome
    let welcome: AdminControlMsg = read_framed(&mut ctrl_recv).await?;
    match welcome {
        AdminControlMsg::Welcome {
            accepted: true, ..
        } => {
            info!("Registered as Online in Admin Dashboard");
            tray.update_status(&format!(
                "RMM Agente — Online ({})",
                config.data.admin_name
            ));
        }
        AdminControlMsg::Welcome {
            accepted: false,
            reason,
            ..
        } => {
            return Err(anyhow!(
                "Admin rejected agent registration: {}",
                reason.unwrap_or_else(|| "unauthorized".into())
            ));
        }
        other => {
            return Err(anyhow!("Unexpected handshake reply: {other:?}"));
        }
    }

    // Channel for sending outbound Control messages safely from multiple tasks/threads
    let (ctrl_tx, mut ctrl_rx) = mpsc::channel::<AgentControlMsg>(64);
    let ctrl_writer = tokio::spawn(async move {
        while let Some(msg) = ctrl_rx.recv().await {
            if let Err(e) = write_framed(&mut ctrl_send, &msg).await {
                warn!("Control stream write failed: {e}");
                break;
            }
        }
    });

    // Shared input injector protected by a fast non-blocking mutex
    let injector = Arc::new(Mutex::new(WinInputInjector::new()));
    if let Some(primary) = displays.iter().find(|d| d.is_primary).or(displays.first()) {
        injector.lock().set_active_display(Some(primary.clone()));
    }

    // Spawn Unreliable Datagram Reader (ultra-low-latency MouseMove)
    let conn_dgram = conn.clone();
    let inj_dgram = injector.clone();
    let dgram_task = tokio::spawn(async move {
        while let Ok(bytes) = conn_dgram.read_datagram().await {
            if bytes.first().copied() == Some(DATAGRAM_MOUSE_MOVE) {
                if let Some(mm) = MouseMoveDatagram::decode(&bytes) {
                    inj_dgram.lock().handle_mouse_move(mm);
                }
            }
        }
    });

    // Spawn Reliable Unidirectional Stream Acceptor (Critical Keyboard / Mouse Buttons)
    let conn_uni = conn.clone();
    let inj_uni = injector.clone();
    let uni_task = tokio::spawn(async move {
        while let Ok(mut recv) = conn_uni.accept_uni().await {
            let inj = inj_uni.clone();
            tokio::spawn(async move {
                while let Ok(ev) = read_framed::<CriticalInputEvent>(&mut recv).await {
                    inj.lock().handle_critical_event(ev);
                }
            });
        }
    });

    // Active video capture/encode worker state
    let mut active_stream: Option<StreamCommandState> = None;

    // Control Stream Reader Loop + User Shutdown watcher
    loop {
        tokio::select! {
            _ = tokio::time::sleep(Duration::from_millis(200)) => {
                if shutdown.load(Ordering::Relaxed) {
                    let _ = ctrl_tx.send(AgentControlMsg::UserDisconnected).await;
                    break;
                }
            }
            msg_res = read_framed::<AdminControlMsg>(&mut ctrl_recv) => {
                let msg = match msg_res {
                    Ok(m) => m,
                    Err(e) => {
                        warn!("Control stream closed: {e}");
                        break;
                    }
                };

                match msg {
                    AdminControlMsg::StartStream {
                        display_index,
                        target_fps,
                        bitrate_kbps,
                        codec,
                        max_width,
                    } => {
                        if let Some(prev) = active_stream.take() {
                            prev.stop_flag.store(true, Ordering::Release);
                        }
                        if let Some(d) = displays.iter().find(|d| d.index == display_index) {
                            injector.lock().set_active_display(Some(d.clone()));
                        }

                        let stop_flag = Arc::new(AtomicBool::new(false));
                        let force_idr = Arc::new(AtomicBool::new(true));
                        let target_bitrate = Arc::new(AtomicU32::new(bitrate_kbps.max(500)));
                        let fps_atomic = Arc::new(AtomicU32::new(target_fps.clamp(10, 120)));

                        active_stream = Some(StreamCommandState {
                            stop_flag: stop_flag.clone(),
                            force_idr: force_idr.clone(),
                            target_bitrate_kbps: target_bitrate.clone(),
                            target_fps: fps_atomic.clone(),
                        });

                        tray.update_status(&format!(
                            "RMM Agente — SESION REMOTA ACTIVA ({})",
                            config.data.admin_name
                        ));

                        spawn_capture_encode_pipeline(
                            conn.clone(),
                            ctrl_tx.clone(),
                            display_index,
                            max_width,
                            codec,
                            stop_flag,
                            force_idr,
                            target_bitrate,
                            fps_atomic,
                        );
                    }
                    AdminControlMsg::StopStream => {
                        if let Some(prev) = active_stream.take() {
                            prev.stop_flag.store(true, Ordering::Release);
                        }
                        injector.lock().release_all();
                        tray.update_status(&format!(
                            "RMM Agente — Online ({})",
                            config.data.admin_name
                        ));
                    }
                    AdminControlMsg::RequestIdr => {
                        if let Some(ref st) = active_stream {
                            st.force_idr.store(true, Ordering::Release);
                        }
                    }
                    AdminControlMsg::AdjustEncoder { bitrate_kbps, target_fps } => {
                        if let Some(ref st) = active_stream {
                            st.target_bitrate_kbps.store(bitrate_kbps.max(500), Ordering::Release);
                            st.target_fps.store(target_fps.clamp(10, 120), Ordering::Release);
                        }
                    }
                    AdminControlMsg::Heartbeat { nonce } => {
                        let _ = ctrl_tx.try_send(AgentControlMsg::HeartbeatAck { nonce });
                    }
                    AdminControlMsg::Disconnect => {
                        info!("Admin requested disconnect");
                        break;
                    }
                    AdminControlMsg::Welcome { .. } => {}
                }
            }
        }
    }

    // Cleanup on disconnect: stop encoder & release any pressed keys/buttons
    if let Some(prev) = active_stream.take() {
        prev.stop_flag.store(true, Ordering::Release);
    }
    injector.lock().release_all();
    dgram_task.abort();
    uni_task.abort();
    drop(ctrl_tx);
    let _ = ctrl_writer.await;
    conn.close(0u32.into(), b"bye");
    Ok(())
}

#[allow(clippy::too_many_arguments)]
fn spawn_capture_encode_pipeline(
    conn: quinn::Connection,
    ctrl_tx: mpsc::Sender<AgentControlMsg>,
    display_index: u32,
    max_width: u32,
    codec: VideoCodec,
    stop_flag: Arc<AtomicBool>,
    force_idr: Arc<AtomicBool>,
    target_bitrate_kbps: Arc<AtomicU32>,
    target_fps: Arc<AtomicU32>,
) {
    // Bounded channel (cap=2) to decouple GPU thread from QUIC packetizer without queue bloat.
    let (frame_tx, frame_rx): (
        Sender<(Vec<u8>, bool, u32)>,
        Receiver<(Vec<u8>, bool, u32)>,
    ) = bounded(2);

    let conn_sender = conn.clone();
    let stop_sender = stop_flag.clone();
    let is_hevc = codec == VideoCodec::Hevc;

    // Datagram sender thread/task
    tokio::task::spawn_blocking(move || {
        let mut frame_id = 0u16;
        let max_dgram = conn_sender
            .max_datagram_size()
            .unwrap_or(DEFAULT_MAX_DATAGRAM_BYTES);

        while !stop_sender.load(Ordering::Relaxed) {
            match frame_rx.recv_timeout(Duration::from_millis(100)) {
                Ok((data, is_keyframe, ts_us)) => {
                    frame_id = frame_id.wrapping_add(1);
                    let mut flags = 0u8;
                    if is_keyframe {
                        flags |= VIDEO_FLAG_KEYFRAME;
                    }
                    if is_hevc {
                        flags |= VIDEO_FLAG_HEVC;
                    }
                    let frags = fragment_video_frame(frame_id, flags, ts_us, &data, max_dgram);
                    for frag in frags {
                        if let Err(e) = conn_sender.send_datagram(frag) {
                            warn!("send_datagram failed: {e}");
                            break;
                        }
                    }
                }
                Err(crossbeam_channel::RecvTimeoutError::Timeout) => continue,
                Err(crossbeam_channel::RecvTimeoutError::Disconnected) => break,
            }
        }
    });

    // Dedicated OS thread for Zero-Copy DXGI Capture + Hardware Encode
    std::thread::Builder::new()
        .name("rmm-gpu-pipeline".into())
        .spawn(move || {
            unsafe {
                let _ = SetThreadPriority(GetCurrentThread(), THREAD_PRIORITY_ABOVE_NORMAL);
            }

            let mut capturer = match DxgiZeroCopyCapturer::new(display_index, max_width) {
                Ok(c) => c,
                Err(e) => {
                    error!("Failed to initialize DXGI capturer: {e:?}");
                    let _ = ctrl_tx.blocking_send(AgentControlMsg::StreamStopped {
                        reason: format!("Capture init error: {e}"),
                    });
                    return;
                }
            };

            let (out_w, out_h) = capturer.output_dimensions();
            let initial_fps = target_fps.load(Ordering::Relaxed);
            let initial_bitrate = target_bitrate_kbps.load(Ordering::Relaxed);

            let mut encoder = match MfHardwareEncoder::new(
                capturer.d3d11_device(),
                out_w,
                out_h,
                initial_fps,
                initial_bitrate,
                codec,
            ) {
                Ok(enc) => enc,
                Err(e) => {
                    error!("Failed to initialize Hardware Encoder: {e:?}");
                    let _ = ctrl_tx.blocking_send(AgentControlMsg::StreamStopped {
                        reason: format!("Encoder init error: {e}"),
                    });
                    return;
                }
            };

            let enc_info = encoder.info().clone();
            let _ = ctrl_tx.blocking_send(AgentControlMsg::StreamStarted {
                display_index,
                width: enc_info.width,
                height: enc_info.height,
                codec: enc_info.codec,
                encoder_name: enc_info.name,
                is_hardware: enc_info.is_hardware,
            });

            let stream_start = Instant::now();
            let mut last_bitrate = initial_bitrate;
            let mut last_cursor_shape_id = 0u64;

            // Telemetry accumulators (1s window)
            let mut stats_window_start = Instant::now();
            let mut frames_in_window = 0u32;
            let mut bytes_in_window = 0u64;
            let mut sum_capture_ms = 0f32;
            let mut sum_encode_ms = 0f32;
            let mut dropped_in_window = 0u32;

            while !stop_flag.load(Ordering::Relaxed) {
                let loop_start = Instant::now();
                let fps_now = target_fps.load(Ordering::Relaxed).max(10);
                let frame_budget = Duration::from_micros(1_000_000 / fps_now as u64);

                let br_now = target_bitrate_kbps.load(Ordering::Relaxed);
                if br_now != last_bitrate {
                    encoder.set_bitrate_kbps(br_now);
                    last_bitrate = br_now;
                }

                let want_idr = force_idr.swap(false, Ordering::AcqRel);
                let timeout_ms = frame_budget.as_millis().clamp(4, 33) as u32;

                match capturer.capture_next_frame(timeout_ms, want_idr) {
                    Ok(CaptureFrameResult::Frame {
                        nv12_texture,
                        capture_ms,
                        cursor_pos,
                        cursor_shape,
                    }) => {
                        send_cursor_updates(
                            &conn,
                            &ctrl_tx,
                            cursor_pos,
                            cursor_shape,
                            &mut last_cursor_shape_id,
                        );

                        match encoder.encode_nv12_texture(&nv12_texture, want_idr) {
                            Ok(encoded_list) => {
                                let ts_us = stream_start.elapsed().as_micros() as u32;
                                for ef in encoded_list {
                                    frames_in_window += 1;
                                    bytes_in_window += ef.data.len() as u64;
                                    sum_capture_ms += capture_ms;
                                    sum_encode_ms += ef.encode_ms;

                                    // Drop-oldest backpressure: never stall GPU waiting on network!
                                    if frame_tx.try_send((ef.data, ef.is_keyframe, ts_us)).is_err() {
                                        dropped_in_window += 1;
                                    }
                                }
                            }
                            Err(e) => {
                                warn!("Encode frame error: {e:?}");
                            }
                        }
                    }
                    Ok(CaptureFrameResult::CursorOnly {
                        cursor_pos,
                        cursor_shape,
                    }) => {
                        send_cursor_updates(
                            &conn,
                            &ctrl_tx,
                            cursor_pos,
                            cursor_shape,
                            &mut last_cursor_shape_id,
                        );
                    }
                    Ok(CaptureFrameResult::Timeout) => {}
                    Ok(CaptureFrameResult::Reinitialized) => {
                        force_idr.store(true, Ordering::Release);
                    }
                    Err(e) => {
                        warn!("Capture loop error: {e:?}");
                        std::thread::sleep(Duration::from_millis(50));
                    }
                }

                // Publish 1-second telemetry snapshot
                let elapsed = stats_window_start.elapsed();
                if elapsed >= Duration::from_secs(1) {
                    let secs = elapsed.as_secs_f32().max(0.001);
                    let denom = frames_in_window.max(1) as f32;
                    let telemetry = AgentControlMsg::Telemetry {
                        fps: frames_in_window as f32 / secs,
                        capture_ms: sum_capture_ms / denom,
                        encode_ms: sum_encode_ms / denom,
                        bitrate_kbps: ((bytes_in_window * 8) as f32 / secs / 1000.0) as u32,
                        dropped_frames: dropped_in_window,
                    };
                    let _ = ctrl_tx.try_send(telemetry);

                    stats_window_start = Instant::now();
                    frames_in_window = 0;
                    bytes_in_window = 0;
                    sum_capture_ms = 0.0;
                    sum_encode_ms = 0.0;
                    dropped_in_window = 0;
                }

                // Pace frame loop precisely
                let spent = loop_start.elapsed();
                if spent < frame_budget {
                    std::thread::sleep(frame_budget - spent);
                }
            }

            let _ = ctrl_tx.blocking_send(AgentControlMsg::StreamStopped {
                reason: "Stopped by Admin".into(),
            });
        })
        .ok();
}

fn send_cursor_updates(
    conn: &quinn::Connection,
    ctrl_tx: &mpsc::Sender<AgentControlMsg>,
    cursor_pos: Option<rmm_capture_win::CursorPositionData>,
    cursor_shape: Option<rmm_capture_win::CursorShapeData>,
    last_shape_id: &mut u64,
) {
    if let Some(shape) = cursor_shape {
        if shape.shape_id != *last_shape_id {
            *last_shape_id = shape.shape_id;
            let _ = ctrl_tx.try_send(AgentControlMsg::CursorShape {
                shape_id: shape.shape_id,
                width: shape.width,
                height: shape.height,
                hotspot_x: shape.hotspot_x,
                hotspot_y: shape.hotspot_y,
                rgba: shape.rgba,
            });
        }
    }
    if let Some(pos) = cursor_pos {
        let dgram = CursorPosDatagram {
            visible: pos.visible,
            x: pos.x,
            y: pos.y,
            shape_id_low: (*last_shape_id & 0xFFFF) as u16,
        };
        let _ = conn.send_datagram(dgram.encode().to_vec().into());
    }
}
