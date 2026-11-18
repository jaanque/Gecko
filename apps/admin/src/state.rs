use std::collections::{HashMap, HashSet};
use std::fs;
use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, AtomicU8, Ordering};
use std::sync::Arc;

use anyhow::Result;
use chrono::{DateTime, Utc};
use crossbeam_channel::Sender;
use eframe::egui;
use parking_lot::{Mutex, RwLock};
use quinn::Connection;
use rmm_decode_win::DecodedFrame;
use rmm_input_win::compute_machine_id;
use rmm_net::AdminIdentity;
use rmm_proto::{AdminControlMsg, CriticalInputEvent, DisplayInfo, VideoCodec};
use serde::{Deserialize, Serialize};
use tokio::sync::mpsc;

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct TelemetrySnapshot {
    pub fps: f32,
    pub capture_ms: f32,
    pub encode_ms: f32,
    pub decode_ms: f32,
    pub bitrate_kbps: u32,
    pub dropped_frames: u32,
}

/// Persistent device record stored on disk so Gecko recognizes returning PCs
/// and shows both Online and known Offline history.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct KnownDeviceRecord {
    pub machine_id: String,
    pub label: String,
    pub hostname: String,
    pub os_user: String,
    pub os_version: String,
    pub gpu_name: String,
    pub last_remote_addr: String,
    pub last_seen: DateTime<Utc>,
}

#[derive(Debug, Clone)]
pub struct AgentSummary {
    pub machine_id: String,
    pub agent_id: String,
    pub token_id: String,
    pub label: String,
    pub hostname: String,
    pub os_user: String,
    pub os_version: String,
    pub gpu_name: String,
    pub remote_addr: String,
    pub displays: Vec<DisplayInfo>,
    pub supported_codecs: Vec<VideoCodec>,
    pub online_since: DateTime<Utc>,
    pub rtt_ms: u32,
    pub streaming: bool,
    pub active_display: u32,
    pub stream_width: u32,
    pub stream_height: u32,
    pub encoder_name: Option<String>,
    pub is_hardware_encoder: bool,
    pub telemetry: TelemetrySnapshot,
    pub is_same_pc: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct IssuedTokenRecord {
    pub token_id: String,
    pub label: String,
    pub endpoints: Vec<String>,
    pub created_at: DateTime<Utc>,
    pub expires_at: Option<DateTime<Utc>>,
    pub output_path: String,
    pub revoked: bool,
}

/// Encoded Access Unit delivered from QUIC Datagram reassembler to the native decoder thread.
pub struct EncodedVideoPacket {
    pub data: Vec<u8>,
    pub is_keyframe: bool,
    pub codec: VideoCodec,
}

pub struct ConnectedAgent {
    pub summary: AgentSummary,
    pub conn: Connection,
    pub ctrl_tx: mpsc::Sender<AdminControlMsg>,
    pub input_tx: mpsc::Sender<CriticalInputEvent>,
    /// When a viewer is actively watching this agent, assembled Annex-B frames are sent here.
    pub video_tx: Arc<Mutex<Option<Sender<EncodedVideoPacket>>>>,
}

/// Shared latest decoded RGBA frame rendered on GPU by egui.
pub struct ViewerFrameBuffer {
    pub latest: Mutex<Option<DecodedFrame>>,
    pub version: AtomicU64,
    pub decode_ms_x10: AtomicU64,
    pub rendered_frames: AtomicU64,
}

impl Default for ViewerFrameBuffer {
    fn default() -> Self {
        Self {
            latest: Mutex::new(None),
            version: AtomicU64::new(0),
            decode_ms_x10: AtomicU64::new(0),
            rendered_frames: AtomicU64::new(0),
        }
    }
}

pub struct AdminState {
    pub identity: AdminIdentity,
    pub local_machine_id: String,
    pub data_dir: PathBuf,
    pub listen_port: u16,
    pub discovery_port: u16,
    /// Keyed by deterministic `machine_id` (deduplicates reconnections cleanly!).
    pub agents: RwLock<HashMap<String, ConnectedAgent>>,
    pub known_devices: RwLock<HashMap<String, KnownDeviceRecord>>,
    pub tokens: RwLock<Vec<IssuedTokenRecord>>,
    pub revoked_tokens: RwLock<HashSet<[u8; 16]>>,
    pub viewer_frame: Arc<ViewerFrameBuffer>,
    pub ui_ctx: RwLock<Option<egui::Context>>,
    pub input_epoch: AtomicU8,
}

impl AdminState {
    pub fn initialize(data_dir: PathBuf, listen_port: u16, discovery_port: u16) -> Result<Self> {
        fs::create_dir_all(&data_dir)?;
        let identity_path = data_dir.join("admin_identity.json");
        let identity = AdminIdentity::load_or_create(&identity_path)?;

        let tokens_path = data_dir.join("issued_tokens.json");
        let tokens: Vec<IssuedTokenRecord> = if tokens_path.exists() {
            fs::read_to_string(&tokens_path)
                .ok()
                .and_then(|s| serde_json::from_str(&s).ok())
                .unwrap_or_default()
        } else {
            Vec::new()
        };

        let devices_path = data_dir.join("known_devices.json");
        let known_list: Vec<KnownDeviceRecord> = if devices_path.exists() {
            fs::read_to_string(&devices_path)
                .ok()
                .and_then(|s| serde_json::from_str(&s).ok())
                .unwrap_or_default()
        } else {
            Vec::new()
        };
        let mut known_devices = HashMap::new();
        for d in known_list {
            known_devices.insert(d.machine_id.clone(), d);
        }

        let mut revoked_set = HashSet::new();
        for t in &tokens {
            if t.revoked {
                if let Ok(u) = uuid::Uuid::parse_str(&t.token_id) {
                    revoked_set.insert(*u.as_bytes());
                }
            }
        }

        let local_machine_id = hex::encode(compute_machine_id());

        Ok(Self {
            identity,
            local_machine_id,
            data_dir,
            listen_port,
            discovery_port,
            agents: RwLock::new(HashMap::new()),
            known_devices: RwLock::new(known_devices),
            tokens: RwLock::new(tokens),
            revoked_tokens: RwLock::new(revoked_set),
            viewer_frame: Arc::new(ViewerFrameBuffer::default()),
            ui_ctx: RwLock::new(None),
            input_epoch: AtomicU8::new(1),
        })
    }

    pub fn set_ui_context(&self, ctx: egui::Context) {
        *self.ui_ctx.write() = Some(ctx);
    }

    pub fn request_repaint(&self) {
        if let Some(ref ctx) = *self.ui_ctx.read() {
            ctx.request_repaint();
        }
    }

    pub fn next_input_epoch(&self) -> u8 {
        self.input_epoch.fetch_add(1, Ordering::Relaxed)
    }

    pub fn persist_tokens(&self) {
        let tokens = self.tokens.read().clone();
        let path = self.data_dir.join("issued_tokens.json");
        if let Ok(json) = serde_json::to_string_pretty(&tokens) {
            let _ = fs::write(path, json);
        }
    }

    pub fn persist_known_devices(&self) {
        let list: Vec<KnownDeviceRecord> = self.known_devices.read().values().cloned().collect();
        let path = self.data_dir.join("known_devices.json");
        if let Ok(json) = serde_json::to_string_pretty(&list) {
            let _ = fs::write(path, json);
        }
    }

    pub fn record_known_device(&self, summary: &AgentSummary) {
        let rec = KnownDeviceRecord {
            machine_id: summary.machine_id.clone(),
            label: summary.label.clone(),
            hostname: summary.hostname.clone(),
            os_user: summary.os_user.clone(),
            os_version: summary.os_version.clone(),
            gpu_name: summary.gpu_name.clone(),
            last_remote_addr: summary.remote_addr.clone(),
            last_seen: Utc::now(),
        };
        self.known_devices
            .write()
            .insert(summary.machine_id.clone(), rec);
        self.persist_known_devices();
    }

    pub fn is_token_revoked(&self, token_id: &[u8; 16]) -> bool {
        self.revoked_tokens.read().contains(token_id)
    }

    pub fn agent_summaries(&self) -> Vec<AgentSummary> {
        let mut list: Vec<AgentSummary> = self
            .agents
            .read()
            .values()
            .map(|a| a.summary.clone())
            .collect();
        list.sort_by(|a, b| b.online_since.cmp(&a.online_since));
        list
    }

    pub fn offline_devices(&self) -> Vec<KnownDeviceRecord> {
        let online_ids: HashSet<String> = self.agents.read().keys().cloned().collect();
        let mut list: Vec<KnownDeviceRecord> = self
            .known_devices
            .read()
            .values()
            .filter(|d| !online_ids.contains(&d.machine_id))
            .cloned()
            .collect();
        list.sort_by(|a, b| b.last_seen.cmp(&a.last_seen));
        list
    }

    pub fn generated_clients_dir(&self) -> PathBuf {
        let dir = self.data_dir.join("generated-clients");
        let _ = fs::create_dir_all(&dir);
        dir
    }

    pub fn revoke_token(&self, token_id: &str) -> Result<()> {
        let parsed = uuid::Uuid::parse_str(token_id)?;
        self.revoked_tokens.write().insert(*parsed.as_bytes());
        for rec in self.tokens.write().iter_mut() {
            if rec.token_id == token_id {
                rec.revoked = true;
            }
        }
        self.persist_tokens();

        let agents = self.agents.read();
        for ag in agents.values() {
            if ag.summary.token_id == token_id {
                let _ = ag.ctrl_tx.try_send(AdminControlMsg::Disconnect);
                ag.conn.close(1u32.into(), b"token revoked");
            }
        }
        self.request_repaint();
        Ok(())
    }

    pub fn disconnect_agent(&self, machine_id: &str) {
        if let Some(ag) = self.agents.read().get(machine_id) {
            let _ = ag.ctrl_tx.try_send(AdminControlMsg::Disconnect);
            ag.conn.close(0u32.into(), b"admin disconnect");
        }
        self.request_repaint();
    }
}
