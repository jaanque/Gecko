pub mod control;
pub mod discovery;
pub mod input;
pub mod overlay;
pub mod video;

pub use control::*;
pub use discovery::*;
pub use input::*;
pub use overlay::*;
pub use video::*;

/// Protocol version checked during QUIC handshake.
pub const PROTOCOL_VERSION: u16 = 2;

/// Application-Layer Protocol Negotiation (ALPN) identifier for QUIC.
pub const ALPN_RMM_V1: &[u8] = b"gecko-rmm-v2";

/// Default UDP port for the Admin QUIC reverse-connection server.
pub const DEFAULT_QUIC_PORT: u16 = 4433;

/// Default UDP port for signed LAN auto-discovery beacon.
pub const DEFAULT_DISCOVERY_PORT: u16 = 4434;
