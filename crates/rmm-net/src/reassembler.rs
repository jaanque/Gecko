use std::collections::HashMap;
use std::time::{Duration, Instant};

use bytes::{BufMut, Bytes, BytesMut};
use rmm_proto::{VideoDatagramHeader, VIDEO_FLAG_END_OF_FRAME};

/// Default safe UDP payload size avoiding IP fragmentation across Internet paths.
pub const DEFAULT_MAX_DATAGRAM_BYTES: usize = 1180;

/// Fragment an encoded H.264/HEVC Access Unit into MTU-safe QUIC datagrams.
pub fn fragment_video_frame(
    frame_id: u16,
    base_flags: u8,
    capture_ts_us: u32,
    payload: &[u8],
    max_datagram_size: usize,
) -> Vec<Bytes> {
    let max_dgram = max_datagram_size.clamp(256, DEFAULT_MAX_DATAGRAM_BYTES);
    let max_chunk = max_dgram - VideoDatagramHeader::WIRE_LEN;

    if payload.is_empty() {
        return Vec::new();
    }

    let chunks: Vec<&[u8]> = payload.chunks(max_chunk).collect();
    let frag_total = chunks.len().min(u16::MAX as usize) as u16;

    let mut out = Vec::with_capacity(chunks.len());
    for (idx, chunk) in chunks.into_iter().take(frag_total as usize).enumerate() {
        let mut flags = base_flags;
        if idx + 1 == frag_total as usize {
            flags |= VIDEO_FLAG_END_OF_FRAME;
        }
        let header = VideoDatagramHeader {
            flags,
            frame_id,
            frag_idx: idx as u16,
            frag_total,
            capture_ts_us,
        };
        let mut buf = BytesMut::with_capacity(VideoDatagramHeader::WIRE_LEN + chunk.len());
        buf.put_slice(&header.encode());
        buf.put_slice(chunk);
        out.push(buf.freeze());
    }
    out
}

#[derive(Debug, Clone)]
pub struct AssembledVideoFrame {
    pub frame_id: u16,
    pub flags: u8,
    pub is_keyframe: bool,
    pub is_hevc: bool,
    pub capture_ts_us: u32,
    pub data: Vec<u8>,
}

#[derive(Debug)]
pub struct ReassembleOutput {
    pub frame: Option<AssembledVideoFrame>,
    pub request_idr: bool,
    pub dropped_frames: u32,
}

struct InFlightFrame {
    flags: u8,
    capture_ts_us: u32,
    frag_total: u16,
    received_count: u16,
    frags: Vec<Option<Vec<u8>>>,
    created_at: Instant,
}

/// Low-latency video frame reassembler for QUIC Unreliable Datagrams.
/// Never blocks waiting for old lost frames; triggers throttled PLI/IDR requests on loss.
pub struct FrameReassembler {
    in_flight: HashMap<u16, InFlightFrame>,
    last_completed_frame: Option<u16>,
    last_idr_request: Option<Instant>,
    idr_cooldown: Duration,
    waiting_for_keyframe: bool,
}

impl Default for FrameReassembler {
    fn default() -> Self {
        Self::new()
    }
}

impl FrameReassembler {
    pub fn new() -> Self {
        Self {
            in_flight: HashMap::new(),
            last_completed_frame: None,
            last_idr_request: None,
            idr_cooldown: Duration::from_millis(300),
            waiting_for_keyframe: true,
        }
    }

    pub fn reset(&mut self) {
        self.in_flight.clear();
        self.last_completed_frame = None;
        self.waiting_for_keyframe = true;
    }

    /// Push a raw QUIC video datagram. Returns an assembled Access Unit when complete.
    pub fn push_datagram(&mut self, raw: &[u8]) -> ReassembleOutput {
        let Some(hdr) = VideoDatagramHeader::decode(raw) else {
            return ReassembleOutput {
                frame: None,
                request_idr: false,
                dropped_frames: 0,
            };
        };
        if hdr.frag_total == 0 || hdr.frag_idx >= hdr.frag_total {
            return ReassembleOutput {
                frame: None,
                request_idr: false,
                dropped_frames: 0,
            };
        }

        // Ignore stale frames older than the last completed frame (wrapping u16 comparison)
        if let Some(last_id) = self.last_completed_frame {
            let diff = hdr.frame_id.wrapping_sub(last_id);
            if diff == 0 || diff > 32_768 {
                return ReassembleOutput {
                    frame: None,
                    request_idr: false,
                    dropped_frames: 0,
                };
            }
        }

        let payload = &raw[VideoDatagramHeader::WIRE_LEN..];
        let now = Instant::now();

        let entry = self
            .in_flight
            .entry(hdr.frame_id)
            .or_insert_with(|| InFlightFrame {
                flags: hdr.flags,
                capture_ts_us: hdr.capture_ts_us,
                frag_total: hdr.frag_total,
                received_count: 0,
                frags: vec![None; hdr.frag_total as usize],
                created_at: now,
            });

        if entry.frag_total != hdr.frag_total {
            return ReassembleOutput {
                frame: None,
                request_idr: false,
                dropped_frames: 0,
            };
        }

        let slot = &mut entry.frags[hdr.frag_idx as usize];
        if slot.is_none() {
            *slot = Some(payload.to_vec());
            entry.received_count += 1;
        }
        entry.flags |= hdr.flags;

        let mut completed = None;
        let mut dropped_frames = 0u32;
        let mut need_idr = false;

        if entry.received_count == entry.frag_total {
            let done = self.in_flight.remove(&hdr.frame_id).unwrap();
            let total_bytes: usize = done
                .frags
                .iter()
                .map(|f| f.as_ref().map_or(0, Vec::len))
                .sum();
            let mut data = Vec::with_capacity(total_bytes);
            for frag in done.frags.into_iter().flatten() {
                data.extend_from_slice(&frag);
            }

            // Detect skipped frames between last_completed_frame and this frame
            if let Some(prev) = self.last_completed_frame {
                let gap = hdr.frame_id.wrapping_sub(prev);
                if gap > 1 && gap < 32_768 {
                    dropped_frames += (gap - 1) as u32;
                    if !hdr.is_keyframe() {
                        self.waiting_for_keyframe = true;
                        need_idr = true;
                    }
                }
            }

            // Drop any older incomplete frames immediately
            let before = self.in_flight.len();
            self.in_flight.retain(|&fid, _| {
                let diff = fid.wrapping_sub(hdr.frame_id);
                diff > 0 && diff < 32_768
            });
            dropped_frames += (before - self.in_flight.len()) as u32;

            self.last_completed_frame = Some(hdr.frame_id);

            let is_kf = hdr.is_keyframe();
            if is_kf {
                self.waiting_for_keyframe = false;
            }

            if !self.waiting_for_keyframe {
                completed = Some(AssembledVideoFrame {
                    frame_id: hdr.frame_id,
                    flags: done.flags,
                    is_keyframe: is_kf,
                    is_hevc: hdr.is_hevc(),
                    capture_ts_us: done.capture_ts_us,
                    data,
                });
            } else {
                need_idr = true;
            }
        } else {
            // Expire incomplete frames older than 120ms so memory stays strictly bounded
            let before = self.in_flight.len();
            self.in_flight
                .retain(|_, f| now.duration_since(f.created_at) < Duration::from_millis(120));
            let expired = before - self.in_flight.len();
            if expired > 0 {
                dropped_frames += expired as u32;
                self.waiting_for_keyframe = true;
                need_idr = true;
            }
        }

        let request_idr = if need_idr {
            let can_request = self
                .last_idr_request
                .map_or(true, |t| now.duration_since(t) >= self.idr_cooldown);
            if can_request {
                self.last_idr_request = Some(now);
                true
            } else {
                false
            }
        } else {
            false
        };

        ReassembleOutput {
            frame: completed,
            request_idr,
            dropped_frames,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use rmm_proto::VIDEO_FLAG_KEYFRAME;

    #[test]
    fn fragments_and_reassembles_out_of_order() {
        let sample = vec![0x5Au8; 5_000];
        let frags = fragment_video_frame(1, VIDEO_FLAG_KEYFRAME, 12345, &sample, 1180);
        assert!(frags.len() >= 4);

        let mut reassembler = FrameReassembler::new();
        // Deliver out of order: reverse order
        let mut assembled = None;
        for f in frags.into_iter().rev() {
            let res = reassembler.push_datagram(&f);
            if res.frame.is_some() {
                assembled = res.frame;
            }
        }
        let frame = assembled.expect("frame should complete");
        assert!(frame.is_keyframe);
        assert_eq!(frame.data, sample);
    }

    #[test]
    fn requests_idr_when_frame_skipped() {
        let mut reassembler = FrameReassembler::new();
        let kf = fragment_video_frame(1, VIDEO_FLAG_KEYFRAME, 100, &[1, 2, 3, 4], 1180);
        let res1 = reassembler.push_datagram(&kf[0]);
        assert!(res1.frame.is_some());

        // Frame 2 lost, frame 3 arrives (non-keyframe)
        let f3 = fragment_video_frame(3, 0, 200, &[5, 6, 7], 1180);
        let res3 = reassembler.push_datagram(&f3[0]);
        assert!(res3.frame.is_none());
        assert!(res3.request_idr);
        assert_eq!(res3.dropped_frames, 1);
    }
}
