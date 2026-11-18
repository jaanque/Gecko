use std::ffi::c_void;
use std::time::Instant;

use anyhow::{anyhow, Context, Result};
use rmm_proto::VideoCodec;
use tracing::{debug, info, warn};
use windows::core::{Interface, GUID, PWSTR, VARIANT};
use windows::Win32::Graphics::Direct3D11::ID3D11Texture2D;
use windows::Win32::Graphics::Direct3D11::ID3D11Device;
use windows::Win32::Media::MediaFoundation::{
    eAVEncCommonRateControlMode_CBR, eAVEncH264VProfile_Main, ICodecAPI, IMF2DBuffer, IMFActivate,
    IMFDXGIDeviceManager, IMFMediaBuffer, IMFMediaEventGenerator, IMFMediaType, IMFSample,
    IMFTransform, MFCreateDXGIDeviceManager, MFCreateDXGISurfaceBuffer, MFCreateMemoryBuffer,
    MFCreateMediaType, MFCreateSample, MFStartup, MFTEnumEx, MFVideoFormat_H264,
    MFVideoFormat_HEVC, MFVideoFormat_NV12, MFVideoInterlace_Progressive,
    CODECAPI_AVEncCommonMeanBitRate, CODECAPI_AVEncCommonQualityVsSpeed,
    CODECAPI_AVEncCommonRateControlMode, CODECAPI_AVEncMPVDefaultBPictureCount,
    CODECAPI_AVEncVideoForceKeyFrame, CODECAPI_AVLowLatencyMode, MFMediaType_Video,
    MFSTARTUP_FULL, MFSampleExtension_CleanPoint, MFT_CATEGORY_VIDEO_ENCODER,
    MFT_ENUM_FLAG_ASYNCMFT, MFT_ENUM_FLAG_HARDWARE, MFT_ENUM_FLAG_SORTANDFILTER,
    MFT_ENUM_FLAG_SYNCMFT, MFT_FRIENDLY_NAME_Attribute, MFT_MESSAGE_NOTIFY_BEGIN_STREAMING,
    MFT_MESSAGE_NOTIFY_END_OF_STREAM, MFT_MESSAGE_NOTIFY_START_OF_STREAM,
    MFT_MESSAGE_SET_D3D_MANAGER, MFT_OUTPUT_DATA_BUFFER, MFT_OUTPUT_STREAM_PROVIDES_SAMPLES,
    MFT_REGISTER_TYPE_INFO, MF_E_TRANSFORM_NEED_MORE_INPUT, MF_E_TRANSFORM_STREAM_CHANGE,
    MF_MT_AVG_BITRATE, MF_MT_FRAME_RATE, MF_MT_FRAME_SIZE, MF_MT_INTERLACE_MODE, MF_MT_MAJOR_TYPE,
    MF_MT_MPEG2_PROFILE, MF_MT_SUBTYPE, MF_TRANSFORM_ASYNC, MF_TRANSFORM_ASYNC_UNLOCK, MF_VERSION,
    METransformHaveOutput, METransformNeedInput, MEDIA_EVENT_GENERATOR_GET_EVENT_FLAGS,
};
use windows::Win32::System::Com::{CoInitializeEx, CoTaskMemFree, COINIT_MULTITHREADED};

#[derive(Debug, Clone)]
pub struct EncodedFrame {
    /// Annex-B H.264 or HEVC bitstream bytes.
    pub data: Vec<u8>,
    pub is_keyframe: bool,
    pub encode_ms: f32,
}

#[derive(Debug, Clone)]
pub struct EncoderInfo {
    pub name: String,
    pub codec: VideoCodec,
    pub is_hardware: bool,
    pub is_async: bool,
    pub width: u32,
    pub height: u32,
}

/// Zero-Copy Windows Media Foundation Hardware Video Encoder (NVENC / AMF / QuickSync).
/// Accepts `ID3D11Texture2D` (NV12) textures residing directly in GPU VRAM.
pub struct MfHardwareEncoder {
    mft: IMFTransform,
    event_gen: Option<IMFMediaEventGenerator>,
    codec_api: Option<ICodecAPI>,
    _dxgi_manager: IMFDXGIDeviceManager,
    info: EncoderInfo,
    fps: u32,
    frame_index: i64,
    mft_provides_samples: bool,
    output_buf_size: u32,
    pending_need_input: u32,
    /// Cached SPS/PPS (H.264) or VPS/SPS/PPS (HEVC) header so every IDR is self-contained for WebCodecs.
    cached_param_sets: Vec<u8>,
}

impl MfHardwareEncoder {
    pub fn new(
        d3d_device: &ID3D11Device,
        width: u32,
        height: u32,
        fps: u32,
        bitrate_kbps: u32,
        codec: VideoCodec,
    ) -> Result<Self> {
        unsafe {
            // COM & Media Foundation initialization (safe to call multiple times per thread)
            let _ = CoInitializeEx(None, COINIT_MULTITHREADED);
            MFStartup(MF_VERSION, MFSTARTUP_FULL).context("MFStartup failed")?;

            let mut reset_token = 0u32;
            let mut dxgi_manager: Option<IMFDXGIDeviceManager> = None;
            MFCreateDXGIDeviceManager(&mut reset_token, &mut dxgi_manager)
                .context("MFCreateDXGIDeviceManager failed")?;
            let dxgi_manager = dxgi_manager.unwrap();
            dxgi_manager
                .ResetDevice(d3d_device, reset_token)
                .context("IMFDXGIDeviceManager::ResetDevice failed")?;

            let out_subtype = match codec {
                VideoCodec::H264 => MFVideoFormat_H264,
                VideoCodec::Hevc => MFVideoFormat_HEVC,
            };

            let (mft, name, is_hardware) = activate_best_encoder(out_subtype)?;
            let attrs = mft.GetAttributes().ok();

            let is_async = attrs
                .as_ref()
                .and_then(|a| a.GetUINT32(&MF_TRANSFORM_ASYNC).ok())
                .unwrap_or(0)
                != 0;

            if is_async {
                if let Some(ref a) = attrs {
                    a.SetUINT32(&MF_TRANSFORM_ASYNC_UNLOCK, 1)
                        .context("Failed to unlock async MFT")?;
                }
            }

            // Bind the shared D3D11 device so the MFT encodes directly from GPU VRAM textures.
            if let Err(e) = mft.ProcessMessage(
                MFT_MESSAGE_SET_D3D_MANAGER,
                dxgi_manager.as_raw() as usize,
            ) {
                warn!(
                    "MFT_MESSAGE_SET_D3D_MANAGER returned {:?} (continuing with DXGI surface buffer)",
                    e
                );
            }

            // Configure output media type (H.264 / HEVC)
            let out_type = create_output_media_type(out_subtype, width, height, fps, bitrate_kbps)?;
            mft.SetOutputType(0, &out_type, 0)
                .context("MFT SetOutputType failed")?;

            // Configure input media type (NV12)
            let in_type = create_input_media_type(width, height, fps)?;
            mft.SetInputType(0, &in_type, 0)
                .context("MFT SetInputType failed")?;

            // Apply ultra-low-latency encoder tuning via ICodecAPI
            let codec_api: Option<ICodecAPI> = mft.cast().ok();
            if let Some(ref api) = codec_api {
                configure_low_latency_codec_api(api, bitrate_kbps);
            }

            let out_stream_info = mft.GetOutputStreamInfo(0)?;
            let mft_provides_samples =
                (out_stream_info.dwFlags & MFT_OUTPUT_STREAM_PROVIDES_SAMPLES.0 as u32) != 0;
            let output_buf_size = out_stream_info.cbSize.max(width * height);

            mft.ProcessMessage(MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0)?;
            mft.ProcessMessage(MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0)?;

            let event_gen = if is_async {
                Some(mft.cast::<IMFMediaEventGenerator>()?)
            } else {
                None
            };

            let info = EncoderInfo {
                name: name.clone(),
                codec,
                is_hardware,
                is_async,
                width,
                height,
            };

            info!(
                encoder = %info.name,
                hardware = info.is_hardware,
                async_mft = info.is_async,
                width,
                height,
                fps,
                bitrate_kbps,
                "Media Foundation encoder initialized"
            );

            Ok(Self {
                mft,
                event_gen,
                codec_api,
                _dxgi_manager: dxgi_manager,
                info,
                fps: fps.max(1),
                frame_index: 0,
                mft_provides_samples,
                output_buf_size,
                pending_need_input: 0,
                cached_param_sets: Vec::new(),
            })
        }
    }

    pub fn info(&self) -> &EncoderInfo {
        &self.info
    }

    /// Dynamically adjust bitrate without restarting the encoder.
    pub fn set_bitrate_kbps(&self, bitrate_kbps: u32) {
        if let Some(ref api) = self.codec_api {
            unsafe {
                let bps = bitrate_kbps.saturating_mul(1000);
                let val = VARIANT::from(bps);
                let _ = api.SetValue(&CODECAPI_AVEncCommonMeanBitRate, &val);
            }
        }
    }

    /// Force the next encoded frame to be an Instantaneous Decoder Refresh (IDR) keyframe.
    pub fn force_keyframe(&self) {
        if let Some(ref api) = self.codec_api {
            unsafe {
                let val = VARIANT::from(1u32);
                let _ = api.SetValue(&CODECAPI_AVEncVideoForceKeyFrame, &val);
            }
        }
    }

    /// Encode a single NV12 VRAM texture without copying pixels to CPU RAM.
    pub fn encode_nv12_texture(
        &mut self,
        nv12_texture: &ID3D11Texture2D,
        force_idr: bool,
    ) -> Result<Vec<EncodedFrame>> {
        let t0 = Instant::now();
        if force_idr || self.frame_index == 0 {
            self.force_keyframe();
        }

        let sample = unsafe { self.wrap_texture_as_sample(nv12_texture)? };
        self.frame_index += 1;

        let mut outputs = Vec::new();
        if self.info.is_async {
            self.encode_async(&sample, t0, &mut outputs)?;
        } else {
            self.encode_sync(&sample, t0, &mut outputs)?;
        }
        Ok(outputs)
    }

    unsafe fn wrap_texture_as_sample(&self, nv12_texture: &ID3D11Texture2D) -> Result<IMFSample> {
        let buffer: IMFMediaBuffer =
            MFCreateDXGISurfaceBuffer(&ID3D11Texture2D::IID, nv12_texture, 0, false)
                .context("MFCreateDXGISurfaceBuffer failed")?;

        if let Ok(buf2d) = buffer.cast::<IMF2DBuffer>() {
            if let Ok(len) = buf2d.GetContiguousLength() {
                let _ = buffer.SetCurrentLength(len);
            }
        }

        let sample = MFCreateSample()?;
        sample.AddBuffer(&buffer)?;

        let duration_hns = 10_000_000i64 / self.fps as i64;
        sample.SetSampleTime(self.frame_index * duration_hns)?;
        sample.SetSampleDuration(duration_hns)?;
        Ok(sample)
    }

    fn encode_sync(
        &mut self,
        sample: &IMFSample,
        t0: Instant,
        outputs: &mut Vec<EncodedFrame>,
    ) -> Result<()> {
        unsafe {
            self.mft
                .ProcessInput(0, sample, 0)
                .context("Sync MFT ProcessInput failed")?;

            loop {
                match self.pull_one_output(t0)? {
                    Some(frame) => outputs.push(frame),
                    None => break,
                }
            }
        }
        Ok(())
    }

    fn encode_async(
        &mut self,
        sample: &IMFSample,
        t0: Instant,
        outputs: &mut Vec<EncodedFrame>,
    ) -> Result<()> {
        let event_gen = self
            .event_gen
            .as_ref()
            .ok_or_else(|| anyhow!("Async MFT missing IMFMediaEventGenerator"))?
            .clone();

        unsafe {
            // Wait until the hardware MFT signals it is ready for input, draining any pending outputs along the way.
            let mut input_fed = false;
            if self.pending_need_input > 0 {
                self.pending_need_input -= 1;
                self.mft
                    .ProcessInput(0, sample, 0)
                    .context("Async MFT ProcessInput failed")?;
                input_fed = true;
            }

            let mut guard_iters = 0u32;
            while guard_iters < 32 {
                guard_iters += 1;
                let event = event_gen.GetEvent(MEDIA_EVENT_GENERATOR_GET_EVENT_FLAGS(0))?;
                let etype = event.GetType()?;

                if etype == METransformNeedInput.0 as u32 {
                    if !input_fed {
                        self.mft
                            .ProcessInput(0, sample, 0)
                            .context("Async MFT ProcessInput failed")?;
                        input_fed = true;
                    } else {
                        self.pending_need_input += 1;
                        if !outputs.is_empty() {
                            break;
                        }
                    }
                } else if etype == METransformHaveOutput.0 as u32 {
                    if let Some(frame) = self.pull_one_output(t0)? {
                        outputs.push(frame);
                    }
                    if input_fed {
                        break;
                    }
                }
            }
        }
        Ok(())
    }

    unsafe fn pull_one_output(&mut self, t0: Instant) -> Result<Option<EncodedFrame>> {
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

        let hr = self.mft.ProcessOutput(0, std::slice::from_mut(&mut out_buf), &mut status);

        let returned_sample = std::mem::ManuallyDrop::take(&mut out_buf.pSample);
        let _returned_events = std::mem::ManuallyDrop::take(&mut out_buf.pEvents);

        if let Err(err) = hr {
            if err.code() == MF_E_TRANSFORM_NEED_MORE_INPUT {
                return Ok(None);
            }
            if err.code() == MF_E_TRANSFORM_STREAM_CHANGE {
                debug!("MFT signaled stream change — refreshing output type");
                if let Ok(new_type) = self.mft.GetOutputAvailableType(0, 0) {
                    let _ = self.mft.SetOutputType(0, &new_type, 0);
                }
                return Ok(None);
            }
            return Err(err.into());
        }

        let Some(sample) = returned_sample else {
            return Ok(None);
        };

        let buf = sample.ConvertToContiguousBuffer()?;
        let mut ptr: *mut u8 = std::ptr::null_mut();
        let mut max_len = 0u32;
        let mut cur_len = 0u32;
        buf.Lock(&mut ptr, Some(&mut max_len), Some(&mut cur_len))?;
        let mut data = if !ptr.is_null() && cur_len > 0 {
            std::slice::from_raw_parts(ptr, cur_len as usize).to_vec()
        } else {
            Vec::new()
        };
        let _ = buf.Unlock();

        if data.is_empty() {
            return Ok(None);
        }

        let clean_point = sample.GetUINT32(&MFSampleExtension_CleanPoint).unwrap_or(0) != 0;
        let (has_param_sets, has_idr) = inspect_annexb_nalus(&data, self.info.codec);
        let is_keyframe = clean_point || has_idr;

        if has_param_sets {
            let extracted = extract_param_set_nalus(&data, self.info.codec);
            if !extracted.is_empty() {
                self.cached_param_sets = extracted;
            }
        } else if is_keyframe && !self.cached_param_sets.is_empty() {
            // Prepend cached SPS/PPS before IDR so WebCodecs VideoDecoder can always initialize mid-stream.
            let mut combined = Vec::with_capacity(self.cached_param_sets.len() + data.len());
            combined.extend_from_slice(&self.cached_param_sets);
            combined.extend_from_slice(&data);
            data = combined;
        }

        Ok(Some(EncodedFrame {
            data,
            is_keyframe,
            encode_ms: t0.elapsed().as_secs_f32() * 1000.0,
        }))
    }
}

impl Drop for MfHardwareEncoder {
    fn drop(&mut self) {
        unsafe {
            let _ = self.mft.ProcessMessage(MFT_MESSAGE_NOTIFY_END_OF_STREAM, 0);
        }
    }
}

unsafe fn activate_best_encoder(out_subtype: GUID) -> Result<(IMFTransform, String, bool)> {
    let out_info = MFT_REGISTER_TYPE_INFO {
        guidMajorType: MFMediaType_Video,
        guidSubtype: out_subtype,
    };

    // 1. Try Hardware Encoders first (NVENC, AMF, Intel QuickSync)
    if let Ok(found) = try_enum_and_activate(
        MFT_ENUM_FLAG_HARDWARE.0 | MFT_ENUM_FLAG_SORTANDFILTER.0,
        &out_info,
    ) {
        return Ok((found.0, found.1, true));
    }

    warn!("No hardware Media Foundation encoder found; falling back to software MFT");

    // 2. Fallback to Software MFT (e.g. headless VMs without physical GPU encoder)
    let found = try_enum_and_activate(
        MFT_ENUM_FLAG_SYNCMFT.0 | MFT_ENUM_FLAG_ASYNCMFT.0 | MFT_ENUM_FLAG_SORTANDFILTER.0,
        &out_info,
    )
    .context("No suitable Media Foundation video encoder available")?;
    Ok((found.0, found.1, false))
}

unsafe fn try_enum_and_activate(
    flags: i32,
    out_info: &MFT_REGISTER_TYPE_INFO,
) -> Result<(IMFTransform, String)> {
    let mut activates_ptr: *mut Option<IMFActivate> = std::ptr::null_mut();
    let mut count = 0u32;

    MFTEnumEx(
        MFT_CATEGORY_VIDEO_ENCODER,
        windows::Win32::Media::MediaFoundation::MFT_ENUM_FLAG(flags),
        None,
        Some(out_info),
        &mut activates_ptr,
        &mut count,
    )?;

    if count == 0 || activates_ptr.is_null() {
        return Err(anyhow!("MFTEnumEx returned 0 encoders"));
    }

    let slice = std::slice::from_raw_parts_mut(activates_ptr, count as usize);
    let mut result: Option<(IMFTransform, String)> = None;

    for slot in slice.iter_mut() {
        if result.is_none() {
            if let Some(ref act) = slot {
                let name = read_friendly_name(act).unwrap_or_else(|| "MF Video Encoder".into());
                if let Ok(mft) = act.ActivateObject::<IMFTransform>() {
                    result = Some((mft, name));
                }
            }
        }
        // Release COM references inside the allocated array
        let _ = slot.take();
    }

    CoTaskMemFree(Some(activates_ptr as *const c_void));
    result.ok_or_else(|| anyhow!("Failed to activate any enumerated MFT"))
}

unsafe fn read_friendly_name(act: &IMFActivate) -> Option<String> {
    let mut ptr = PWSTR::null();
    let mut len = 0u32;
    if act
        .GetAllocatedString(&MFT_FRIENDLY_NAME_Attribute, &mut ptr, &mut len)
        .is_ok()
        && !ptr.is_null()
    {
        let s = ptr.to_string().ok();
        CoTaskMemFree(Some(ptr.0 as *const c_void));
        return s;
    }
    None
}

unsafe fn create_output_media_type(
    subtype: GUID,
    width: u32,
    height: u32,
    fps: u32,
    bitrate_kbps: u32,
) -> Result<IMFMediaType> {
    let mt = MFCreateMediaType()?;
    mt.SetGUID(&MF_MT_MAJOR_TYPE, &MFMediaType_Video)?;
    mt.SetGUID(&MF_MT_SUBTYPE, &subtype)?;
    mt.SetUINT32(&MF_MT_AVG_BITRATE, bitrate_kbps.saturating_mul(1000))?;
    mt.SetUINT32(&MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive.0 as u32)?;
    mt.SetUINT64(&MF_MT_FRAME_SIZE, pack_u32_pair(width, height))?;
    mt.SetUINT64(&MF_MT_FRAME_RATE, pack_u32_pair(fps.max(1), 1))?;
    if subtype == MFVideoFormat_H264 {
        mt.SetUINT32(&MF_MT_MPEG2_PROFILE, eAVEncH264VProfile_Main.0 as u32)?;
    }
    Ok(mt)
}

unsafe fn create_input_media_type(width: u32, height: u32, fps: u32) -> Result<IMFMediaType> {
    let mt = MFCreateMediaType()?;
    mt.SetGUID(&MF_MT_MAJOR_TYPE, &MFMediaType_Video)?;
    mt.SetGUID(&MF_MT_SUBTYPE, &MFVideoFormat_NV12)?;
    mt.SetUINT32(&MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive.0 as u32)?;
    mt.SetUINT64(&MF_MT_FRAME_SIZE, pack_u32_pair(width, height))?;
    mt.SetUINT64(&MF_MT_FRAME_RATE, pack_u32_pair(fps.max(1), 1))?;
    Ok(mt)
}

unsafe fn configure_low_latency_codec_api(api: &ICodecAPI, bitrate_kbps: u32) {
    let _ = api.SetValue(&CODECAPI_AVLowLatencyMode, &VARIANT::from(true));
    let _ = api.SetValue(
        &CODECAPI_AVEncCommonRateControlMode,
        &VARIANT::from(eAVEncCommonRateControlMode_CBR.0 as u32),
    );
    let _ = api.SetValue(&CODECAPI_AVEncMPVDefaultBPictureCount, &VARIANT::from(0u32));
    let _ = api.SetValue(
        &CODECAPI_AVEncCommonMeanBitRate,
        &VARIANT::from(bitrate_kbps.saturating_mul(1000)),
    );
    let _ = api.SetValue(&CODECAPI_AVEncCommonQualityVsSpeed, &VARIANT::from(20u32));
}

#[inline]
fn pack_u32_pair(high: u32, low: u32) -> u64 {
    ((high as u64) << 32) | (low as u64)
}

/// Inspect Annex-B NAL units to detect parameter sets (SPS/PPS/VPS) and IDR keyframes.
fn inspect_annexb_nalus(data: &[u8], codec: VideoCodec) -> (bool, bool) {
    let mut has_param = false;
    let mut has_idr = false;
    let mut i = 0;
    while i + 4 < data.len() {
        let start_len = if data[i] == 0 && data[i + 1] == 0 && data[i + 2] == 1 {
            3
        } else if data[i] == 0 && data[i + 1] == 0 && data[i + 2] == 0 && data[i + 3] == 1 {
            4
        } else {
            i += 1;
            continue;
        };
        let nal_byte = data[i + start_len];
        match codec {
            VideoCodec::H264 => {
                let nal_type = nal_byte & 0x1F;
                if nal_type == 7 || nal_type == 8 {
                    has_param = true;
                } else if nal_type == 5 {
                    has_idr = true;
                }
            }
            VideoCodec::Hevc => {
                let nal_type = (nal_byte >> 1) & 0x3F;
                if nal_type == 32 || nal_type == 33 || nal_type == 34 {
                    has_param = true;
                } else if nal_type == 19 || nal_type == 20 {
                    has_idr = true;
                }
            }
        }
        i += start_len + 1;
    }
    (has_param, has_idr)
}

/// Extract parameter set NALUs (SPS/PPS or VPS/SPS/PPS) in Annex-B format.
fn extract_param_set_nalus(data: &[u8], codec: VideoCodec) -> Vec<u8> {
    let mut starts = Vec::new();
    let mut i = 0;
    while i + 3 < data.len() {
        if i + 4 <= data.len()
            && data[i] == 0
            && data[i + 1] == 0
            && data[i + 2] == 0
            && data[i + 3] == 1
        {
            starts.push((i, 4usize));
            i += 4;
        } else if data[i] == 0 && data[i + 1] == 0 && data[i + 2] == 1 {
            starts.push((i, 3usize));
            i += 3;
        } else {
            i += 1;
        }
    }

    let mut out = Vec::new();
    for (idx, &(pos, sc_len)) in starts.iter().enumerate() {
        if pos + sc_len >= data.len() {
            continue;
        }
        let end = starts.get(idx + 1).map_or(data.len(), |&(p, _)| p);
        let nal_byte = data[pos + sc_len];
        let is_param = match codec {
            VideoCodec::H264 => {
                let t = nal_byte & 0x1F;
                t == 7 || t == 8
            }
            VideoCodec::Hevc => {
                let t = (nal_byte >> 1) & 0x3F;
                t == 32 || t == 33 || t == 34
            }
        };
        if is_param {
            out.extend_from_slice(&data[pos..end]);
        }
    }
    out
}
