use serde::{Deserialize, Serialize};

/// Describes one physical display attached to the remote client.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct DisplayInfo {
    pub index: u32,
    pub name: String,
    pub width: u32,
    pub height: u32,
    pub left: i32,
    pub top: i32,
    pub is_primary: bool,
}

/// Hardware video codec negotiated between Admin and Agent.
#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
pub enum VideoCodec {
    H264,
    Hevc,
}

/// Messages sent from the Remote Agent to the Admin on QUIC Control Stream #0.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub enum AgentControlMsg {
    Hello {
        protocol_version: u16,
        /// Stable deterministic hardware/machine fingerprint so Admin recognizes returning PCs.
        machine_id: [u8; 16],
        agent_id: [u8; 16],
        token_id: [u8; 16],
        label: String,
        hostname: String,
        os_user: String,
        os_version: String,
        gpu_name: String,
        displays: Vec<DisplayInfo>,
        supported_codecs: Vec<VideoCodec>,
        signed_token: Vec<u8>,
    },
    StreamStarted {
        display_index: u32,
        width: u32,
        height: u32,
        codec: VideoCodec,
        encoder_name: String,
        is_hardware: bool,
    },
    StreamStopped {
        reason: String,
    },
    Telemetry {
        fps: f32,
        encode_ms: f32,
        capture_ms: f32,
        bitrate_kbps: u32,
        dropped_frames: u32,
    },
    CursorShape {
        shape_id: u64,
        width: u32,
        height: u32,
        hotspot_x: i32,
        hotspot_y: i32,
        /// RGBA8 premultiplied cursor bitmap (compact)
        rgba: Vec<u8>,
    },
    HeartbeatAck {
        nonce: u64,
    },
    UserDisconnected,
}

/// Messages sent from the Admin to the Remote Agent on QUIC Control Stream #0.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub enum AdminControlMsg {
    Welcome {
        accepted: bool,
        reason: Option<String>,
        session_id: [u8; 16],
    },
    StartStream {
        display_index: u32,
        target_fps: u32,
        bitrate_kbps: u32,
        codec: VideoCodec,
        /// Optional downscale cap (0 = native resolution)
        max_width: u32,
    },
    StopStream,
    /// Request an immediate Instantaneous Decoder Refresh (IDR) keyframe after packet loss.
    RequestIdr,
    /// Dynamic Adaptive Bitrate adjustment without restarting the stream.
    AdjustEncoder {
        bitrate_kbps: u32,
        target_fps: u32,
    },
    Heartbeat {
        nonce: u64,
    },
    Disconnect,
}
