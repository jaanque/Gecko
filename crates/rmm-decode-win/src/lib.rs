use std::ffi::c_void;
use std::time::Instant;

use anyhow::{anyhow, Context, Result};
use rmm_proto::VideoCodec;
use tracing::{debug, info};
use windows::core::{Interface, GUID, VARIANT};
use windows::Win32::Media::MediaFoundation::{
    ICodecAPI, IMF2DBuffer, IMFActivate, IMFMediaType, IMFTransform,
    MFCreateMemoryBuffer, MFCreateMediaType, MFCreateSample, MFStartup, MFTEnumEx,
    MFVideoFormat_H264, MFVideoFormat_HEVC, MFVideoFormat_NV12, CODECAPI_AVLowLatencyMode,
    MFMediaType_Video, MFSTARTUP_FULL, MFT_CATEGORY_VIDEO_DECODER, MFT_ENUM_FLAG,
    MFT_ENUM_FLAG_SORTANDFILTER, MFT_ENUM_FLAG_SYNCMFT, MFT_MESSAGE_COMMAND_FLUSH, MFT_MESSAGE_NOTIFY_BEGIN_STREAMING,
    MFT_MESSAGE_NOTIFY_END_OF_STREAM, MFT_MESSAGE_NOTIFY_START_OF_STREAM, MFT_OUTPUT_DATA_BUFFER,
    MFT_OUTPUT_STREAM_PROVIDES_SAMPLES, MFT_REGISTER_TYPE_INFO, MFVideoArea,
    MF_E_TRANSFORM_NEED_MORE_INPUT, MF_E_TRANSFORM_STREAM_CHANGE, MF_MT_DEFAULT_STRIDE,
    MF_MT_FRAME_SIZE, MF_MT_MAJOR_TYPE, MF_MT_MINIMUM_DISPLAY_APERTURE, MF_MT_SUBTYPE, MF_VERSION,
};
use windows::Win32::System::Com::{CoInitializeEx, CoTaskMemFree, COINIT_MULTITHREADED};

#[derive(Debug, Clone)]
pub struct DecodedFrame {
    pub width: u32,
    pub height: u32,
    /// Tightly packed RGBA8 pixel data (width * height * 4 bytes) ready for GPU upload.
    pub rgba: Vec<u8>,
    pub decode_ms: f32,
}

/// Native Windows Media Foundation H.264 / HEVC Video Decoder with ultra-low-latency mode.
pub struct MfVideoDecoder {
    mft: IMFTransform,
    codec: VideoCodec,
    coded_width: u32,
    coded_height: u32,
    display_width: u32,
    display_height: u32,
    stride: i32,
    mft_provides_samples: bool,
    output_buf_size: u32,
    frame_index: i64,
}

impl MfVideoDecoder {
    pub fn new(codec: VideoCodec) -> Result<Self> {
        unsafe {
            let _ = CoInitializeEx(None, COINIT_MULTITHREADED);
            MFStartup(MF_VERSION, MFSTARTUP_FULL).context("MFStartup failed in decoder")?;

            let in_subtype = match codec {
                VideoCodec::H264 => MFVideoFormat_H264,
                VideoCodec::Hevc => MFVideoFormat_HEVC,
            };

            let mft = activate_decoder(in_subtype)?;

            // Enable ultra-low-latency decoding (disables frame reordering buffer!)
            if let Ok(attrs) = mft.GetAttributes() {
                let _ = attrs.SetUINT32(&CODECAPI_AVLowLatencyMode, 1);
            }
            if let Ok(api) = mft.cast::<ICodecAPI>() {
                let _ = api.SetValue(&CODECAPI_AVLowLatencyMode, &VARIANT::from(true));
            }

            // Configure input type
            let in_type = MFCreateMediaType()?;
            in_type.SetGUID(&MF_MT_MAJOR_TYPE, &MFMediaType_Video)?;
            in_type.SetGUID(&MF_MT_SUBTYPE, &in_subtype)?;
            mft.SetInputType(0, &in_type, 0)
                .context("Decoder SetInputType failed")?;

            let mut dec = Self {
                mft,
                codec,
                coded_width: 0,
                coded_height: 0,
                display_width: 0,
                display_height: 0,
                stride: 0,
                mft_provides_samples: false,
                output_buf_size: 1920 * 1080 * 2,
                frame_index: 0,
            };

            dec.configure_nv12_output()?;

            dec.mft
                .ProcessMessage(MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0)?;
            dec.mft
                .ProcessMessage(MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0)?;

            info!(?codec, "Native Media Foundation video decoder initialized");
            Ok(dec)
        }
    }

    pub fn codec(&self) -> VideoCodec {
        self.codec
    }

    pub fn flush(&mut self) {
        unsafe {
            let _ = self.mft.ProcessMessage(MFT_MESSAGE_COMMAND_FLUSH, 0);
        }
    }

    /// Decode an Annex-B Access Unit into zero or more tightly-packed RGBA8 frames.
    pub fn decode(&mut self, annexb: &[u8]) -> Result<Vec<DecodedFrame>> {
        if annexb.is_empty() {
            return Ok(Vec::new());
        }
        let t0 = Instant::now();
        let mut results = Vec::new();

        unsafe {
            let mem = MFCreateMemoryBuffer(annexb.len() as u32)?;
            let mut ptr: *mut u8 = std::ptr::null_mut();
            mem.Lock(&mut ptr, None, None)?;
            std::ptr::copy_nonoverlapping(annexb.as_ptr(), ptr, annexb.len());
            mem.Unlock()?;
            mem.SetCurrentLength(annexb.len() as u32)?;

            let sample = MFCreateSample()?;
            sample.AddBuffer(&mem)?;
            sample.SetSampleTime(self.frame_index * 166_667)?;
            sample.SetSampleDuration(166_667)?;
            self.frame_index += 1;

            self.mft
                .ProcessInput(0, &sample, 0)
                .context("Decoder ProcessInput failed")?;

            loop {
                match self.pull_output(t0)? {
                    PullState::Frame(frame) => results.push(frame),
                    PullState::StreamChanged => continue,
                    PullState::NeedMoreInput => break,
                }
            }
        }

        Ok(results)
    }

    unsafe fn configure_nv12_output(&mut self) -> Result<()> {
        let mut idx = 0u32;
        let mut chosen: Option<IMFMediaType> = None;

        while let Ok(mt) = self.mft.GetOutputAvailableType(0, idx) {
            idx += 1;
            if let Ok(sub) = mt.GetGUID(&MF_MT_SUBTYPE) {
                if sub == MFVideoFormat_NV12 {
                    chosen = Some(mt);
                    break;
                }
            }
        }

        let mt = chosen.ok_or_else(|| anyhow!("Decoder does not expose NV12 output"))?;
        self.mft
            .SetOutputType(0, &mt, 0)
            .context("Decoder SetOutputType(NV12) failed")?;

        if let Ok(packed) = mt.GetUINT64(&MF_MT_FRAME_SIZE) {
            self.coded_width = (packed >> 32) as u32;
            self.coded_height = (packed & 0xFFFF_FFFF) as u32;
        }

        self.display_width = self.coded_width;
        self.display_height = self.coded_height;

        // Respect H.264 cropping rectangle (e.g. 1920x1088 coded -> 1920x1080 visible display)
        let mut area_bytes = [0u8; std::mem::size_of::<MFVideoArea>()];
        if mt
            .GetBlob(&MF_MT_MINIMUM_DISPLAY_APERTURE, &mut area_bytes, None)
            .is_ok()
        {
            let area: MFVideoArea =
                std::ptr::read_unaligned(area_bytes.as_ptr() as *const MFVideoArea);
            if area.Area.cx > 0 && area.Area.cy > 0 {
                self.display_width = area.Area.cx as u32;
                self.display_height = area.Area.cy as u32;
            }
        }

        self.stride = mt
            .GetUINT32(&MF_MT_DEFAULT_STRIDE)
            .map(|v| v as i32)
            .unwrap_or(self.coded_width as i32)
            .abs();
        if self.stride == 0 {
            self.stride = self.coded_width as i32;
        }

        let info = self.mft.GetOutputStreamInfo(0)?;
        self.mft_provides_samples =
            (info.dwFlags & MFT_OUTPUT_STREAM_PROVIDES_SAMPLES.0 as u32) != 0;
        let min_nv12 = (self.stride as u32) * self.coded_height.max(16) * 3 / 2;
        self.output_buf_size = info.cbSize.max(min_nv12).max(1920 * 1080 * 2);

        debug!(
            coded_w = self.coded_width,
            coded_h = self.coded_height,
            disp_w = self.display_width,
            disp_h = self.display_height,
            stride = self.stride,
            mft_provides = self.mft_provides_samples,
            "Decoder NV12 output type configured"
        );
        Ok(())
    }

    unsafe fn pull_output(&mut self, t0: Instant) -> Result<PullState> {
        let allocated_sample = if self.mft_provides_samples {
            None
        } else {
            let s = MFCreateSample()?;
            let mem = MFCreateMemoryBuffer(self.output_buf_size)?;
            s.AddBuffer(&mem)?;
            Some(s)
        };

        let mut out_buf = MFT_OUTPUT_DATA_BUFFER {
            dwStreamID: 0,
            pSample: std::mem::ManuallyDrop::new(allocated_sample),
            dwStatus: 0,
            pEvents: std::mem::ManuallyDrop::new(None),
        };
        let mut status = 0u32;

        let hr = self
            .mft
            .ProcessOutput(0, std::slice::from_mut(&mut out_buf), &mut status);

        let returned_sample = std::mem::ManuallyDrop::take(&mut out_buf.pSample);
        let _returned_events = std::mem::ManuallyDrop::take(&mut out_buf.pEvents);

        if let Err(err) = hr {
            if err.code() == MF_E_TRANSFORM_NEED_MORE_INPUT {
                return Ok(PullState::NeedMoreInput);
            }
            if err.code() == MF_E_TRANSFORM_STREAM_CHANGE {
                self.configure_nv12_output()?;
                return Ok(PullState::StreamChanged);
            }
            return Err(err.into());
        }

        let Some(sample) = returned_sample else {
            return Ok(PullState::NeedMoreInput);
        };

        let w = self.display_width as usize;
        let h = self.display_height as usize;
        let coded_h = self.coded_height.max(self.display_height) as usize;
        if w == 0 || h == 0 {
            return Ok(PullState::NeedMoreInput);
        }

        let buf = sample.ConvertToContiguousBuffer()?;
        let mut rgba = vec![255u8; w * h * 4];

        if let Ok(buf2d) = buf.cast::<IMF2DBuffer>() {
            let mut scan0: *mut u8 = std::ptr::null_mut();
            let mut pitch: i32 = 0;
            if buf2d.Lock2D(&mut scan0, &mut pitch).is_ok() && !scan0.is_null() {
                let stride = pitch.unsigned_abs() as usize;
                let total_bytes = stride * coded_h * 3 / 2;
                let slice = std::slice::from_raw_parts(scan0, total_bytes);
                nv12_to_rgba(slice, stride, coded_h, w, h, &mut rgba);
                let _ = buf2d.Unlock2D();
                return Ok(PullState::Frame(DecodedFrame {
                    width: w as u32,
                    height: h as u32,
                    rgba,
                    decode_ms: t0.elapsed().as_secs_f32() * 1000.0,
                }));
            }
        }

        let mut ptr: *mut u8 = std::ptr::null_mut();
        let mut max_len = 0u32;
        let mut cur_len = 0u32;
        buf.Lock(&mut ptr, Some(&mut max_len), Some(&mut cur_len))?;
        if !ptr.is_null() && cur_len > 0 {
            let slice = std::slice::from_raw_parts(ptr, cur_len as usize);
            let stride = (self.stride as usize).max(w);
            nv12_to_rgba(slice, stride, coded_h, w, h, &mut rgba);
        }
        let _ = buf.Unlock();

        Ok(PullState::Frame(DecodedFrame {
            width: w as u32,
            height: h as u32,
            rgba,
            decode_ms: t0.elapsed().as_secs_f32() * 1000.0,
        }))
    }
}

impl Drop for MfVideoDecoder {
    fn drop(&mut self) {
        unsafe {
            let _ = self.mft.ProcessMessage(MFT_MESSAGE_NOTIFY_END_OF_STREAM, 0);
        }
    }
}

enum PullState {
    Frame(DecodedFrame),
    StreamChanged,
    NeedMoreInput,
}

unsafe fn activate_decoder(in_subtype: GUID) -> Result<IMFTransform> {
    let in_info = MFT_REGISTER_TYPE_INFO {
        guidMajorType: MFMediaType_Video,
        guidSubtype: in_subtype,
    };

    // Prefer Sync MFT (e.g. Microsoft H.264/HEVC Video Decoder MFT) which supports
    // CODECAPI_AVLowLatencyMode and direct synchronous ProcessInput/ProcessOutput.
    let flags = MFT_ENUM_FLAG_SYNCMFT.0 | MFT_ENUM_FLAG_SORTANDFILTER.0;

    let mut activates_ptr: *mut Option<IMFActivate> = std::ptr::null_mut();
    let mut count = 0u32;

    MFTEnumEx(
        MFT_CATEGORY_VIDEO_DECODER,
        MFT_ENUM_FLAG(flags),
        Some(&in_info),
        None,
        &mut activates_ptr,
        &mut count,
    )?;

    if count == 0 || activates_ptr.is_null() {
        return Err(anyhow!("No Media Foundation video decoder found"));
    }

    let slice = std::slice::from_raw_parts_mut(activates_ptr, count as usize);
    let mut result: Option<IMFTransform> = None;

    for slot in slice.iter_mut() {
        if result.is_none() {
            if let Some(ref act) = slot {
                if let Ok(mft) = act.ActivateObject::<IMFTransform>() {
                    result = Some(mft);
                }
            }
        }
        let _ = slot.take();
    }

    CoTaskMemFree(Some(activates_ptr as *const c_void));
    result.ok_or_else(|| anyhow!("Failed to activate any video decoder MFT"))
}

/// Fast integer BT.601/BT.709 NV12 -> RGBA8 conversion.
fn nv12_to_rgba(
    nv12: &[u8],
    stride: usize,
    coded_h: usize,
    w: usize,
    h: usize,
    out_rgba: &mut [u8],
) {
    let uv_offset = stride * coded_h;
    if nv12.len() < uv_offset + stride * (h / 2) {
        return;
    }

    for y in 0..h {
        let y_row = y * stride;
        let uv_row = uv_offset + (y >> 1) * stride;
        let dst_row = y * w * 4;

        for x in 0..w {
            let y_val = nv12[y_row + x] as i32;
            let uv_idx = uv_row + (x & !1);
            let u = nv12[uv_idx] as i32 - 128;
            let v = nv12[uv_idx + 1] as i32 - 128;

            let c = (y_val - 16).max(0) * 298;
            let r = ((c + 409 * v + 128) >> 8).clamp(0, 255) as u8;
            let g = ((c - 100 * u - 208 * v + 128) >> 8).clamp(0, 255) as u8;
            let b = ((c + 516 * u + 128) >> 8).clamp(0, 255) as u8;

            let dst = dst_row + x * 4;
            out_rgba[dst] = r;
            out_rgba[dst + 1] = g;
            out_rgba[dst + 2] = b;
            out_rgba[dst + 3] = 255;
        }
    }
}
