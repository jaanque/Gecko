use std::fs;
use std::net::UdpSocket;
use std::path::{Path, PathBuf};

use anyhow::{anyhow, Context, Result};
use chrono::{DateTime, Duration, Utc};
use rmm_proto::{inject_config_into_stub, AgentConfigData, SignedAgentConfig};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

use crate::state::{AdminState, IssuedTokenRecord};

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct StubStatus {
    pub found: bool,
    pub path: Option<String>,
    pub size_bytes: u64,
}

#[derive(Debug, Clone, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct GenerateClientRequest {
    pub label: String,
    pub admin_name: String,
    pub endpoints: Vec<String>,
    /// Expiration in hours. `0` or `None` means no expiration.
    pub ttl_hours: Option<u64>,
    pub file_name: Option<String>,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct GenerateClientResponse {
    pub token_id: String,
    pub output_path: String,
    pub size_bytes: usize,
    pub endpoints: Vec<String>,
    pub expires_at: Option<DateTime<Utc>>,
}

/// Locate the precompiled `rmm-agent.exe` / `agent_stub.exe` template binary.
/// Searches bundled resource paths first, then workspace Cargo target folders (for dev).
pub fn locate_agent_stub(app_resource_dir: Option<&Path>) -> Option<PathBuf> {
    let mut candidates = Vec::new();

    if let Some(res) = app_resource_dir {
        candidates.push(res.join("agent_stub.exe"));
        candidates.push(res.join("resources").join("agent_stub.exe"));
    }

    if let Ok(exe) = std::env::current_exe() {
        if let Some(dir) = exe.parent() {
            candidates.push(dir.join("agent_stub.exe"));
            candidates.push(dir.join("rmm-agent.exe"));
            candidates.push(dir.join("resources").join("agent_stub.exe"));
            // Walk up to workspace root in cargo dev/release layouts
            for ancestor in dir.ancestors().take(5) {
                candidates.push(ancestor.join("target").join("release-agent").join("rmm-agent.exe"));
                candidates.push(ancestor.join("target").join("release").join("rmm-agent.exe"));
                candidates.push(ancestor.join("target").join("debug").join("rmm-agent.exe"));
                candidates.push(
                    ancestor
                        .join("apps")
                        .join("admin")
                        .join("src-tauri")
                        .join("resources")
                        .join("agent_stub.exe"),
                );
            }
        }
    }

    if let Ok(cwd) = std::env::current_dir() {
        for ancestor in cwd.ancestors().take(4) {
            candidates.push(ancestor.join("target").join("release-agent").join("rmm-agent.exe"));
            candidates.push(ancestor.join("target").join("release").join("rmm-agent.exe"));
            candidates.push(ancestor.join("target").join("debug").join("rmm-agent.exe"));
        }
    }

    candidates.into_iter().find(|p| p.is_file())
}

pub fn get_stub_status(app_resource_dir: Option<&Path>) -> StubStatus {
    match locate_agent_stub(app_resource_dir) {
        Some(path) => {
            let size = fs::metadata(&path).map(|m| m.len()).unwrap_or(0);
            StubStatus {
                found: true,
                path: Some(path.to_string_lossy().to_string()),
                size_bytes: size,
            }
        }
        None => StubStatus {
            found: false,
            path: None,
            size_bytes: 0,
        },
    }
}

/// Detect the primary LAN IPv4 address of this Admin machine (zero external network traffic).
pub fn detect_local_ipv4() -> Vec<String> {
    let mut ips = Vec::new();
    if let Ok(sock) = UdpSocket::bind("0.0.0.0:0") {
        if sock.connect("1.1.1.1:80").is_ok() {
            if let Ok(addr) = sock.local_addr() {
                ips.push(addr.ip().to_string());
            }
        }
    }
    if !ips.iter().any(|ip| ip == "127.0.0.1") {
        ips.push("127.0.0.1".to_string());
    }
    ips
}

/// Generate a customized, zero-config single-file executable by injecting a cryptographically
/// signed configuration overlay at the tail of the precompiled agent stub.
pub fn generate_configured_client(
    state: &AdminState,
    app_resource_dir: Option<&Path>,
    req: GenerateClientRequest,
) -> Result<GenerateClientResponse> {
    let stub_path = locate_agent_stub(app_resource_dir).ok_or_else(|| {
        anyhow!(
            "No se encontro la plantilla precompilada (rmm-agent.exe). Compila primero el agente con `cargo build -p rmm-agent --profile release-agent`."
        )
    })?;

    let stub_bytes = fs::read(&stub_path)
        .with_context(|| format!("Failed to read stub binary at {}", stub_path.display()))?;

    let endpoints: Vec<String> = req
        .endpoints
        .into_iter()
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .collect();
    if endpoints.is_empty() {
        return Err(anyhow!("Debes indicar al menos un endpoint (IP:puerto o dominio:puerto)"));
    }

    let token_uuid = Uuid::new_v4();
    let now = Utc::now();
    let expires_at = req
        .ttl_hours
        .filter(|&h| h > 0)
        .map(|h| now + Duration::hours(h as i64));

    let label = if req.label.trim().is_empty() {
        format!("Cliente-{}", &token_uuid.to_string()[..6])
    } else {
        req.label.trim().to_string()
    };

    let admin_name = if req.admin_name.trim().is_empty() {
        "Gecko Admin".to_string()
    } else {
        req.admin_name.trim().to_string()
    };

    let config_data = AgentConfigData {
        endpoints: endpoints.clone(),
        admin_cert_sha256: state.identity.cert_sha256,
        admin_verify_key: state.identity.verifying_key().to_bytes(),
        token_id: *token_uuid.as_bytes(),
        expires_at: expires_at.map_or(0, |dt| dt.timestamp().max(0) as u64),
        label: label.clone(),
        admin_name,
    };

    let signed = SignedAgentConfig::sign(config_data, &state.identity.signing_key)?;

    let sanitized_name = req
        .file_name
        .as_deref()
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(sanitize_filename)
        .unwrap_or_else(|| format!("Gecko-{}.exe", sanitize_filename(&label)));

    let final_name = if sanitized_name.to_lowercase().ends_with(".exe") {
        sanitized_name
    } else {
        format!("{sanitized_name}.exe")
    };

    let out_dir = state.generated_clients_dir();
    let output_path = out_dir.join(final_name);

    let size_bytes = inject_config_into_stub(&stub_bytes, &signed, &output_path)?;

    let record = IssuedTokenRecord {
        token_id: token_uuid.to_string(),
        label,
        endpoints: endpoints.clone(),
        created_at: now,
        expires_at,
        output_path: output_path.to_string_lossy().to_string(),
        revoked: false,
    };

    state.tokens.write().insert(0, record);
    state.persist_tokens();

    Ok(GenerateClientResponse {
        token_id: token_uuid.to_string(),
        output_path: output_path.to_string_lossy().to_string(),
        size_bytes,
        endpoints,
        expires_at,
    })
}

fn sanitize_filename(raw: &str) -> String {
    raw.chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() || c == '-' || c == '_' || c == '.' {
                c
            } else {
                '_'
            }
        })
        .collect()
}
