use std::fs::{self, File};
use std::io::{Read, Seek, SeekFrom, Write};
use std::path::Path;

use ed25519_dalek::{Signature, Signer, SigningKey, Verifier, VerifyingKey};
use serde::{Deserialize, Serialize};
use thiserror::Error;

pub const OVERLAY_MAGIC_START: &[u8; 8] = b"RMMCFG01";
pub const OVERLAY_MAGIC_END: &[u8; 4] = b"END!";
pub const OVERLAY_FORMAT_VERSION: u16 = 1;
pub const OVERLAY_FOOTER_LEN: usize = 24;
pub const OVERLAY_MAX_PAYLOAD_LEN: usize = 65_536;

#[derive(Debug, Error)]
pub enum OverlayError {
    #[error("I/O error: {0}")]
    Io(#[from] std::io::Error),
    #[error("No embedded RMM configuration overlay found in executable")]
    NotFound,
    #[error("Unsupported overlay format version: {0}")]
    UnsupportedVersion(u16),
    #[error("Invalid overlay payload length: {0}")]
    InvalidLength(usize),
    #[error("Overlay CRC32 mismatch (corrupted binary)")]
    CrcMismatch,
    #[error("Serialization error: {0}")]
    Serialization(#[from] postcard::Error),
    #[error("Invalid Ed25519 public key or signature")]
    InvalidSignature,
    #[error("Configuration token has expired")]
    TokenExpired,
}

/// Unsigned configuration fields signed by the Admin's Ed25519 identity key.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct AgentConfigData {
    /// Ordered list of Admin QUIC endpoints (e.g. "203.0.113.10:4433", "192.168.1.5:4433").
    pub endpoints: Vec<String>,
    /// SHA-256 hash of the Admin server's TLS certificate SPKI/DER for pinning (no CA needed).
    pub admin_cert_sha256: [u8; 32],
    /// Admin's Ed25519 verifying key (used by agent to verify signed config integrity).
    pub admin_verify_key: [u8; 32],
    /// Unique token ID (UUID bytes) allowing individual revocation in Admin dashboard.
    pub token_id: [u8; 16],
    /// Unix timestamp (seconds) when this enrollment token expires. 0 = never expires.
    pub expires_at: u64,
    /// Human-readable label chosen by Admin (e.g. "Soporte - Cliente ACME").
    pub label: String,
    /// Optional Admin display name shown in the client tray/banner for user transparency.
    pub admin_name: String,
}

/// Complete signed configuration block appended as a PE overlay.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct SignedAgentConfig {
    pub data: AgentConfigData,
    /// Ed25519 signature over `postcard::to_allocvec(&data)`.
    pub signature: Vec<u8>,
}

impl SignedAgentConfig {
    /// Sign `AgentConfigData` with the Admin's Ed25519 signing key.
    pub fn sign(data: AgentConfigData, signing_key: &SigningKey) -> Result<Self, OverlayError> {
        let canonical = postcard::to_allocvec(&data)?;
        let sig = signing_key.sign(&canonical);
        Ok(Self {
            data,
            signature: sig.to_bytes().to_vec(),
        })
    }

    /// Verify signature integrity against embedded `admin_verify_key` (or an expected key).
    pub fn verify(&self, expected_verify_key: Option<&[u8; 32]>) -> Result<(), OverlayError> {
        let key_bytes = expected_verify_key.unwrap_or(&self.data.admin_verify_key);
        if let Some(expected) = expected_verify_key {
            if &self.data.admin_verify_key != expected {
                return Err(OverlayError::InvalidSignature);
            }
        }
        let verifying_key =
            VerifyingKey::from_bytes(key_bytes).map_err(|_| OverlayError::InvalidSignature)?;
        let sig_bytes: [u8; 64] = self
            .signature
            .as_slice()
            .try_into()
            .map_err(|_| OverlayError::InvalidSignature)?;
        let sig = Signature::from_bytes(&sig_bytes);
        let canonical = postcard::to_allocvec(&self.data)?;
        verifying_key
            .verify(&canonical, &sig)
            .map_err(|_| OverlayError::InvalidSignature)?;
        Ok(())
    }

    /// Check whether this config's enrollment token has expired relative to `now_unix_secs`.
    pub fn check_not_expired(&self, now_unix_secs: u64) -> Result<(), OverlayError> {
        if self.data.expires_at != 0 && now_unix_secs > self.data.expires_at {
            return Err(OverlayError::TokenExpired);
        }
        Ok(())
    }
}

/// Encode a `SignedAgentConfig` into the overlay payload + 24-byte footer.
pub fn encode_overlay_trailer(config: &SignedAgentConfig) -> Result<Vec<u8>, OverlayError> {
    let payload = postcard::to_allocvec(config)?;
    if payload.len() > OVERLAY_MAX_PAYLOAD_LEN {
        return Err(OverlayError::InvalidLength(payload.len()));
    }
    let crc = crc32fast::hash(&payload);

    let mut out = Vec::with_capacity(payload.len() + OVERLAY_FOOTER_LEN);
    out.extend_from_slice(&payload);
    out.extend_from_slice(OVERLAY_MAGIC_START);
    out.extend_from_slice(&OVERLAY_FORMAT_VERSION.to_le_bytes());
    out.extend_from_slice(&0u16.to_le_bytes()); // reserved flags
    out.extend_from_slice(&(payload.len() as u32).to_le_bytes());
    out.extend_from_slice(&crc.to_le_bytes());
    out.extend_from_slice(OVERLAY_MAGIC_END);
    Ok(out)
}

/// Take a precompiled stub executable, strip any prior overlay if present,
/// append the new signed configuration overlay, and write the ready-to-run client binary.
pub fn inject_config_into_stub(
    stub_bytes: &[u8],
    config: &SignedAgentConfig,
    output_path: &Path,
) -> Result<usize, OverlayError> {
    let base_len = stripped_stub_len(stub_bytes);
    let trailer = encode_overlay_trailer(config)?;

    if let Some(parent) = output_path.parent() {
        fs::create_dir_all(parent)?;
    }

    let mut file = File::create(output_path)?;
    file.write_all(&stub_bytes[..base_len])?;
    file.write_all(&trailer)?;
    file.flush()?;

    Ok(base_len + trailer.len())
}

/// If `stub_bytes` already has a valid overlay trailer, return the length without it
/// so re-injection is idempotent.
fn stripped_stub_len(bytes: &[u8]) -> usize {
    if bytes.len() < OVERLAY_FOOTER_LEN {
        return bytes.len();
    }
    let footer = &bytes[bytes.len() - OVERLAY_FOOTER_LEN..];
    if &footer[0..8] == OVERLAY_MAGIC_START && &footer[20..24] == OVERLAY_MAGIC_END {
        let payload_len = u32::from_le_bytes(footer[12..16].try_into().unwrap()) as usize;
        if payload_len <= OVERLAY_MAX_PAYLOAD_LEN
            && bytes.len() >= OVERLAY_FOOTER_LEN + payload_len
        {
            return bytes.len() - OVERLAY_FOOTER_LEN - payload_len;
        }
    }
    bytes.len()
}

/// Read and verify an embedded configuration overlay from any executable file path.
pub fn read_config_from_binary(path: &Path) -> Result<SignedAgentConfig, OverlayError> {
    let mut file = File::open(path)?;
    let file_len = file.metadata()?.len();
    if file_len < OVERLAY_FOOTER_LEN as u64 {
        return Err(OverlayError::NotFound);
    }

    file.seek(SeekFrom::End(-(OVERLAY_FOOTER_LEN as i64)))?;
    let mut footer = [0u8; OVERLAY_FOOTER_LEN];
    file.read_exact(&mut footer)?;

    if &footer[0..8] != OVERLAY_MAGIC_START || &footer[20..24] != OVERLAY_MAGIC_END {
        return Err(OverlayError::NotFound);
    }

    let version = u16::from_le_bytes(footer[8..10].try_into().unwrap());
    if version != OVERLAY_FORMAT_VERSION {
        return Err(OverlayError::UnsupportedVersion(version));
    }

    let payload_len = u32::from_le_bytes(footer[12..16].try_into().unwrap()) as usize;
    let expected_crc = u32::from_le_bytes(footer[16..20].try_into().unwrap());

    if payload_len == 0
        || payload_len > OVERLAY_MAX_PAYLOAD_LEN
        || (payload_len + OVERLAY_FOOTER_LEN) as u64 > file_len
    {
        return Err(OverlayError::InvalidLength(payload_len));
    }

    let offset = -((OVERLAY_FOOTER_LEN + payload_len) as i64);
    file.seek(SeekFrom::End(offset))?;
    let mut payload = vec![0u8; payload_len];
    file.read_exact(&mut payload)?;

    let actual_crc = crc32fast::hash(&payload);
    if actual_crc != expected_crc {
        return Err(OverlayError::CrcMismatch);
    }

    let config: SignedAgentConfig = postcard::from_bytes(&payload)?;
    config.verify(None)?;
    Ok(config)
}

/// Convenience helper for the Agent to read its own injected configuration at startup.
pub fn read_config_from_current_exe() -> Result<SignedAgentConfig, OverlayError> {
    let exe_path = std::env::current_exe()?;
    read_config_from_binary(&exe_path)
}

#[cfg(test)]
mod tests {
    use super::*;
    use rand::rngs::OsRng;

    #[test]
    fn roundtrip_inject_and_read_overlay() {
        let mut csprng = OsRng;
        let signing_key = SigningKey::generate(&mut csprng);
        let verify_key = signing_key.verifying_key();

        let data = AgentConfigData {
            endpoints: vec!["192.168.1.50:4433".into(), "admin.example.com:4433".into()],
            admin_cert_sha256: [0xAB; 32],
            admin_verify_key: verify_key.to_bytes(),
            token_id: [0x11; 16],
            expires_at: 0,
            label: "ACME Corp - Recepcion".into(),
            admin_name: "Soporte IT".into(),
        };

        let signed = SignedAgentConfig::sign(data.clone(), &signing_key).unwrap();
        let fake_pe = b"MZ\x90\x00FAKE_PE_STUB_CONTENT_1234567890".to_vec();

        let dir = tempfile::tempdir().unwrap();
        let out_path = dir.path().join("client_acme.exe");

        let written = inject_config_into_stub(&fake_pe, &signed, &out_path).unwrap();
        assert!(written > fake_pe.len());

        let loaded = read_config_from_binary(&out_path).unwrap();
        assert_eq!(loaded.data, data);
        loaded.verify(Some(&verify_key.to_bytes())).unwrap();

        // Re-injecting into an already-injected binary must stay idempotent in size
        let already_injected = fs::read(&out_path).unwrap();
        let out_path_2 = dir.path().join("client_acme_2.exe");
        let written_2 = inject_config_into_stub(&already_injected, &signed, &out_path_2).unwrap();
        assert_eq!(written, written_2);
    }

    #[test]
    fn detects_tampered_payload() {
        let mut csprng = OsRng;
        let signing_key = SigningKey::generate(&mut csprng);
        let verify_key = signing_key.verifying_key();

        let data = AgentConfigData {
            endpoints: vec!["127.0.0.1:4433".into()],
            admin_cert_sha256: [0xCD; 32],
            admin_verify_key: verify_key.to_bytes(),
            token_id: [0x22; 16],
            expires_at: 0,
            label: "Test".into(),
            admin_name: "Admin".into(),
        };
        let signed = SignedAgentConfig::sign(data, &signing_key).unwrap();
        let dir = tempfile::tempdir().unwrap();
        let out_path = dir.path().join("tampered.exe");
        inject_config_into_stub(b"MZ_STUB", &signed, &out_path).unwrap();

        let mut bytes = fs::read(&out_path).unwrap();
        bytes[10] ^= 0xFF; // flip a byte inside payload
        fs::write(&out_path, &bytes).unwrap();

        let err = read_config_from_binary(&out_path).unwrap_err();
        assert!(matches!(err, OverlayError::CrcMismatch));
    }
}
