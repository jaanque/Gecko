use std::fs;
use std::path::Path;

use anyhow::{Context, Result};
use base64::engine::general_purpose::STANDARD as B64;
use base64::Engine;
use ed25519_dalek::{SigningKey, VerifyingKey};
use rand::rngs::OsRng;
use rcgen::{CertificateParams, DnType, KeyPair};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

/// Persistent cryptographic identity of the Admin Server (Ed25519 + TLS 1.3 certificate).
#[derive(Clone)]
pub struct AdminIdentity {
    pub signing_key: SigningKey,
    pub cert_der: Vec<u8>,
    pub key_der: Vec<u8>,
    pub cert_sha256: [u8; 32],
}

#[derive(Serialize, Deserialize)]
struct StoredIdentity {
    signing_key_b64: String,
    cert_der_b64: String,
    key_der_b64: String,
}

impl AdminIdentity {
    /// Load an existing Admin identity from `path`, or generate & persist a fresh one.
    pub fn load_or_create(path: &Path) -> Result<Self> {
        if path.exists() {
            let text = fs::read_to_string(path).context("Failed to read Admin identity file")?;
            let stored: StoredIdentity =
                serde_json::from_str(&text).context("Invalid Admin identity JSON")?;
            let sk_bytes: [u8; 32] = B64
                .decode(&stored.signing_key_b64)?
                .try_into()
                .map_err(|_| anyhow::anyhow!("Invalid Ed25519 key length"))?;
            let cert_der = B64.decode(&stored.cert_der_b64)?;
            let key_der = B64.decode(&stored.key_der_b64)?;
            let cert_sha256 = sha256_bytes(&cert_der);
            return Ok(Self {
                signing_key: SigningKey::from_bytes(&sk_bytes),
                cert_der,
                key_der,
                cert_sha256,
            });
        }

        let identity = Self::generate()?;
        identity.save(path)?;
        Ok(identity)
    }

    pub fn generate() -> Result<Self> {
        let mut csprng = OsRng;
        let signing_key = SigningKey::generate(&mut csprng);

        let mut params = CertificateParams::new(vec!["gecko-admin.local".to_string()])?;
        params
            .distinguished_name
            .push(DnType::CommonName, "Gecko Admin");
        let key_pair = KeyPair::generate()?;
        let cert = params.self_signed(&key_pair)?;
        let cert_der = cert.der().to_vec();
        let key_der = key_pair.serialize_der();
        let cert_sha256 = sha256_bytes(&cert_der);

        Ok(Self {
            signing_key,
            cert_der,
            key_der,
            cert_sha256,
        })
    }

    pub fn save(&self, path: &Path) -> Result<()> {
        if let Some(parent) = path.parent() {
            fs::create_dir_all(parent)?;
        }
        let stored = StoredIdentity {
            signing_key_b64: B64.encode(self.signing_key.to_bytes()),
            cert_der_b64: B64.encode(&self.cert_der),
            key_der_b64: B64.encode(&self.key_der),
        };
        let json = serde_json::to_string_pretty(&stored)?;
        fs::write(path, json)?;
        Ok(())
    }

    pub fn verifying_key(&self) -> VerifyingKey {
        self.signing_key.verifying_key()
    }

    pub fn cert_fingerprint_hex(&self) -> String {
        hex::encode(self.cert_sha256)
    }
}

pub fn sha256_bytes(data: &[u8]) -> [u8; 32] {
    let mut hasher = Sha256::new();
    hasher.update(data);
    hasher.finalize().into()
}
