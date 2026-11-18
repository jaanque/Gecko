/// Compact 24-byte UDP LAN Discovery Probe sent by an Agent when configured
/// Admin endpoints are unreachable (e.g. Admin changed local Wi-Fi/DHCP IP).
/// Wire layout:
/// - [0..8]   b"GECKOD01"
/// - [8..24]  first 16 bytes of `admin_cert_sha256`
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct LanDiscoveryProbe {
    pub cert_prefix: [u8; 16],
}

impl LanDiscoveryProbe {
    pub const MAGIC: &'static [u8; 8] = b"GECKOD01";
    pub const WIRE_LEN: usize = 24;

    pub fn from_cert_sha256(sha256: &[u8; 32]) -> Self {
        let mut cert_prefix = [0u8; 16];
        cert_prefix.copy_from_slice(&sha256[..16]);
        Self { cert_prefix }
    }

    pub fn encode(&self) -> [u8; Self::WIRE_LEN] {
        let mut out = [0u8; Self::WIRE_LEN];
        out[0..8].copy_from_slice(Self::MAGIC);
        out[8..24].copy_from_slice(&self.cert_prefix);
        out
    }

    pub fn decode(buf: &[u8]) -> Option<Self> {
        if buf.len() < Self::WIRE_LEN || &buf[0..8] != Self::MAGIC {
            return None;
        }
        let mut cert_prefix = [0u8; 16];
        cert_prefix.copy_from_slice(&buf[8..24]);
        Some(Self { cert_prefix })
    }
}

/// Compact 26-byte UDP LAN Discovery Reply sent back by the matching Gecko Admin.
/// Wire layout:
/// - [0..8]   b"GECKOR01"
/// - [8..24]  first 16 bytes of `admin_cert_sha256`
/// - [24..26] quic_port (u16 LE)
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct LanDiscoveryReply {
    pub cert_prefix: [u8; 16],
    pub quic_port: u16,
}

impl LanDiscoveryReply {
    pub const MAGIC: &'static [u8; 8] = b"GECKOR01";
    pub const WIRE_LEN: usize = 26;

    pub fn encode(&self) -> [u8; Self::WIRE_LEN] {
        let mut out = [0u8; Self::WIRE_LEN];
        out[0..8].copy_from_slice(Self::MAGIC);
        out[8..24].copy_from_slice(&self.cert_prefix);
        out[24..26].copy_from_slice(&self.quic_port.to_le_bytes());
        out
    }

    pub fn decode(buf: &[u8]) -> Option<Self> {
        if buf.len() < Self::WIRE_LEN || &buf[0..8] != Self::MAGIC {
            return None;
        }
        let mut cert_prefix = [0u8; 16];
        cert_prefix.copy_from_slice(&buf[8..24]);
        let quic_port = u16::from_le_bytes([buf[24], buf[25]]);
        Some(Self {
            cert_prefix,
            quic_port,
        })
    }
}
