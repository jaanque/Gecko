pub mod control;
pub mod input;
pub mod overlay;
pub mod video;

pub use control::*;
pub use input::*;
pub use overlay::*;
pub use video::*;

/// Protocol version checked during QUIC handshake.
pub const PROTOCOL_VERSION: u16 = 1;

/// Application-Layer Protocol Negotiation (ALPN) identifier for QUIC.
pub const ALPN_RMM_V1: &[u8] = b"rmm-zero-v1";
