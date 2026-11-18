use std::collections::{HashMap, HashSet};
use std::fs;
use std::path::PathBuf;
use std::sync::Arc;

use anyhow::Result;
use chrono::{DateTime, Utc};
use parking_lot::{Mutex, RwLock};
use quinn::Connection;
use rmm_net::AdminIdentity;
use rmm_proto::{AdminControlMsg, CriticalInputEvent, DisplayInfo, VideoCodec};
use serde::{Deserialize, Serialize};
use tauri::ipc::{Channel, InvokeResponseBody};
use tokio::sync::mpsc;

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(rename_all = "camelCase")]
pub struct TelemetrySnapshot {
    pub fps: f32,
    pub capture_ms: f32,
    pub encode_ms: f32,
    pub bitrate_kbps: u32,
    pub dropped_frames: u32,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct AgentSummary {
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
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct IssuedTokenRecord {
    pub token_id: String,
    pub label: String,
    pub endpoints: Vec<String>,
    pub created_at: DateTime<Utc>,
    pub expires_at: Option<DateTime<Utc>>,
    pub output_path: String,
    pub revoked: bool,
}

pub struct ConnectedAgent {
    pub summary: AgentSummary,
    pub conn: Connection,
    pub ctrl_tx: mpsc::Sender<AdminControlMsg>,
    pub input_tx: mpsc::Sender<CriticalInputEvent>,
    pub video_channel: Arc<Mutex<Option<Channel<InvokeResponseBody>>>>,
}

pub struct AdminState {
    pub identity: AdminIdentity,
    pub data_dir: PathBuf,
    pub listen_port: u16,
    pub agents: RwLock<HashMap<String, ConnectedAgent>>,
    pub tokens: RwLock<Vec<IssuedTokenRecord>>,
    pub revoked_tokens: RwLock<HashSet<[u8; 16]>>,
}

impl AdminState {
    pub fn initialize(data_dir: PathBuf, listen_port: u16) -> Result<Self> {
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

        let mut revoked_set = HashSet::new();
        for t in &tokens {
            if t.revoked {
                if let Ok(u) = uuid::Uuid::parse_str(&t.token_id) {
                    revoked_set.insert(*u.as_bytes());
                }
            }
        }

        Ok(Self {
            identity,
            data_dir,
            listen_port,
            agents: RwLock::new(HashMap::new()),
            tokens: RwLock::new(tokens),
            revoked_tokens: RwLock::new(revoked_set),
        })
    }

    pub fn persist_tokens(&self) {
        let tokens = self.tokens.read().clone();
        let path = self.data_dir.join("issued_tokens.json");
        if let Ok(json) = serde_json::to_string_pretty(&tokens) {
            let _ = fs::write(path, json);
        }
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

    pub fn generated_clients_dir(&self) -> PathBuf {
        let dir = self.data_dir.join("generated-clients");
        let _ = fs::create_dir_all(&dir);
        dir
    }
}
