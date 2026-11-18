use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq, Hash)]
pub enum MouseButton {
    Left,
    Right,
    Middle,
    X1,
    X2,
}

/// Critical input events sent over a dedicated high-priority RELIABLE QUIC stream
/// so key-up / button-up events are never dropped (preventing stuck keys).
#[derive(Debug, Clone, Serialize, Deserialize)]
pub enum CriticalInputEvent {
    /// Hardware PS/2 Set 1 scan code (layout-independent).
    Key {
        scancode: u16,
        extended: bool,
        pressed: bool,
    },
    MouseButton {
        button: MouseButton,
        pressed: bool,
        /// Optional absolute normalized position (0..65535) at click instant.
        x: u16,
        y: u16,
    },
    MouseWheel {
        /// Positive = scroll up / right, negative = scroll down / left (in wheel notches * 120).
        delta_x: i16,
        delta_y: i16,
    },
    /// Releases all currently held keys/buttons (e.g. when viewer loses focus).
    ReleaseAll,
}

/// Ultra-low-latency mouse movement sent as an UNRELIABLE QUIC datagram (no HoL blocking).
/// Fixed 9 bytes on wire: `[0x02, display_idx(u8), epoch(u8), x_le(2), y_le(2), seq_le(2)]`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct MouseMoveDatagram {
    pub display_index: u8,
    /// Session epoch incremented every time a remote control session starts.
    pub epoch: u8,
    /// Normalized 0..65535 coordinate within the active display.
    pub x: u16,
    /// Normalized 0..65535 coordinate within the active display.
    pub y: u16,
    /// Monotonic sequence number so stale out-of-order datagrams are ignored.
    pub seq: u16,
}

impl MouseMoveDatagram {
    pub const WIRE_LEN: usize = 9;
    pub const MSG_TYPE: u8 = 0x02;

    pub fn encode(&self) -> [u8; Self::WIRE_LEN] {
        let mut buf = [0u8; Self::WIRE_LEN];
        buf[0] = Self::MSG_TYPE;
        buf[1] = self.display_index;
        buf[2] = self.epoch;
        buf[3..5].copy_from_slice(&self.x.to_le_bytes());
        buf[5..7].copy_from_slice(&self.y.to_le_bytes());
        buf[7..9].copy_from_slice(&self.seq.to_le_bytes());
        buf
    }

    pub fn decode(buf: &[u8]) -> Option<Self> {
        if buf.len() < Self::WIRE_LEN || buf[0] != Self::MSG_TYPE {
            return None;
        }
        Some(Self {
            display_index: buf[1],
            epoch: buf[2],
            x: u16::from_le_bytes([buf[3], buf[4]]),
            y: u16::from_le_bytes([buf[5], buf[6]]),
            seq: u16::from_le_bytes([buf[7], buf[8]]),
        })
    }
}

/// Cursor position update from Agent -> Admin via unreliable datagram.
/// Fixed 8 bytes: `[0x03, visible(1), x_le(2), y_le(2), shape_hash_low(2)]`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct CursorPosDatagram {
    pub visible: bool,
    pub x: u16,
    pub y: u16,
    pub shape_id_low: u16,
}

impl CursorPosDatagram {
    pub const WIRE_LEN: usize = 8;
    pub const MSG_TYPE: u8 = 0x03;

    pub fn encode(&self) -> [u8; Self::WIRE_LEN] {
        let mut buf = [0u8; Self::WIRE_LEN];
        buf[0] = Self::MSG_TYPE;
        buf[1] = u8::from(self.visible);
        buf[2..4].copy_from_slice(&self.x.to_le_bytes());
        buf[4..6].copy_from_slice(&self.y.to_le_bytes());
        buf[6..8].copy_from_slice(&self.shape_id_low.to_le_bytes());
        buf
    }

    pub fn decode(buf: &[u8]) -> Option<Self> {
        if buf.len() < Self::WIRE_LEN || buf[0] != Self::MSG_TYPE {
            return None;
        }
        Some(Self {
            visible: buf[1] != 0,
            x: u16::from_le_bytes([buf[2], buf[3]]),
            y: u16::from_le_bytes([buf[4], buf[5]]),
            shape_id_low: u16::from_le_bytes([buf[6], buf[7]]),
        })
    }
}
