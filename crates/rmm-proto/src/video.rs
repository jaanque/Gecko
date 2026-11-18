/// First byte discriminator on QUIC Unreliable Datagrams.
pub const DATAGRAM_VIDEO_FRAG: u8 = 0x01;
pub const DATAGRAM_MOUSE_MOVE: u8 = 0x02;
pub const DATAGRAM_CURSOR_POS: u8 = 0x03;

pub const VIDEO_FLAG_KEYFRAME: u8 = 1 << 0;
pub const VIDEO_FLAG_HEVC: u8 = 1 << 1;
pub const VIDEO_FLAG_END_OF_FRAME: u8 = 1 << 2;

/// Compact 12-byte header prefixed to every MTU-sized H.264/HEVC video datagram.
/// Layout (little-endian):
/// - [0]      msg_type = 0x01
/// - [1]      flags (bit0=keyframe, bit1=hevc, bit2=eof)
/// - [2..4]   frame_id: u16
/// - [4..6]   frag_idx: u16
/// - [6..8]   frag_total: u16
/// - [8..12]  capture_ts_us: u32 (wrapping microsecond timestamp for latency tracking)
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct VideoDatagramHeader {
    pub flags: u8,
    pub frame_id: u16,
    pub frag_idx: u16,
    pub frag_total: u16,
    pub capture_ts_us: u32,
}

impl VideoDatagramHeader {
    pub const WIRE_LEN: usize = 12;

    pub fn is_keyframe(&self) -> bool {
        (self.flags & VIDEO_FLAG_KEYFRAME) != 0
    }

    pub fn is_hevc(&self) -> bool {
        (self.flags & VIDEO_FLAG_HEVC) != 0
    }

    pub fn encode(&self) -> [u8; Self::WIRE_LEN] {
        let mut buf = [0u8; Self::WIRE_LEN];
        buf[0] = DATAGRAM_VIDEO_FRAG;
        buf[1] = self.flags;
        buf[2..4].copy_from_slice(&self.frame_id.to_le_bytes());
        buf[4..6].copy_from_slice(&self.frag_idx.to_le_bytes());
        buf[6..8].copy_from_slice(&self.frag_total.to_le_bytes());
        buf[8..12].copy_from_slice(&self.capture_ts_us.to_le_bytes());
        buf
    }

    pub fn decode(buf: &[u8]) -> Option<Self> {
        if buf.len() < Self::WIRE_LEN || buf[0] != DATAGRAM_VIDEO_FRAG {
            return None;
        }
        Some(Self {
            flags: buf[1],
            frame_id: u16::from_le_bytes([buf[2], buf[3]]),
            frag_idx: u16::from_le_bytes([buf[4], buf[5]]),
            frag_total: u16::from_le_bytes([buf[6], buf[7]]),
            capture_ts_us: u32::from_le_bytes([buf[8], buf[9], buf[10], buf[11]]),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn video_header_roundtrip() {
        let hdr = VideoDatagramHeader {
            flags: VIDEO_FLAG_KEYFRAME | VIDEO_FLAG_END_OF_FRAME,
            frame_id: 4219,
            frag_idx: 3,
            frag_total: 4,
            capture_ts_us: 987_654_321,
        };
        let raw = hdr.encode();
        let decoded = VideoDatagramHeader::decode(&raw).unwrap();
        assert_eq!(hdr, decoded);
        assert!(decoded.is_keyframe());
        assert!(!decoded.is_hevc());
    }
}
