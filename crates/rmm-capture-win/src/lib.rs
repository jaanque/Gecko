use std::time::Instant;

use anyhow::{anyhow, Context, Result};
use rmm_proto::DisplayInfo;
use tracing::{debug, warn};
use windows::core::Interface;
use windows::Win32::Foundation::RECT;
use windows::Win32::Graphics::Direct3D::{D3D_DRIVER_TYPE_UNKNOWN, D3D_FEATURE_LEVEL_11_0};
use windows::Win32::Graphics::Direct3D11::{
    D3D11CreateDevice, ID3D11Device, ID3D11DeviceContext, ID3D11Multithread, ID3D11Texture2D,
    ID3D11VideoContext, ID3D11VideoDevice, ID3D11VideoProcessor, ID3D11VideoProcessorEnumerator,
    ID3D11VideoProcessorInputView, ID3D11VideoProcessorOutputView, D3D11_BIND_RENDER_TARGET,
    D3D11_BIND_SHADER_RESOURCE, D3D11_CREATE_DEVICE_BGRA_SUPPORT,
    D3D11_CREATE_DEVICE_VIDEO_SUPPORT, D3D11_SDK_VERSION, D3D11_TEXTURE2D_DESC,
    D3D11_USAGE_DEFAULT, D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE,
    D3D11_VIDEO_PROCESSOR_CONTENT_DESC, D3D11_VIDEO_PROCESSOR_INPUT_VIEW_DESC,
    D3D11_VIDEO_PROCESSOR_INPUT_VIEW_DESC_0, D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC,
    D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC_0, D3D11_VIDEO_PROCESSOR_STREAM,
    D3D11_VIDEO_USAGE_PLAYBACK_NORMAL, D3D11_VPIV_DIMENSION_TEXTURE2D,
    D3D11_VPOV_DIMENSION_TEXTURE2D, D3D11_TEX2D_VPIV, D3D11_TEX2D_VPOV,
};
use windows::Win32::Graphics::Dxgi::Common::{DXGI_FORMAT_NV12, DXGI_RATIONAL, DXGI_SAMPLE_DESC};
use windows::Win32::Graphics::Dxgi::{
    CreateDXGIFactory1, IDXGIAdapter1, IDXGIFactory1, IDXGIOutput1, IDXGIOutputDuplication,
    IDXGIResource, DXGI_ERROR_ACCESS_LOST, DXGI_ERROR_NOT_FOUND, DXGI_ERROR_WAIT_TIMEOUT,
    DXGI_OUTDUPL_FRAME_INFO, DXGI_OUTDUPL_POINTER_SHAPE_INFO,
    DXGI_OUTDUPL_POINTER_SHAPE_TYPE_COLOR, DXGI_OUTDUPL_POINTER_SHAPE_TYPE_MASKED_COLOR,
    DXGI_OUTDUPL_POINTER_SHAPE_TYPE_MONOCHROME,
};

#[derive(Debug, Clone)]
pub struct CursorShapeData {
    pub shape_id: u64,
    pub width: u32,
    pub height: u32,
    pub hotspot_x: i32,
    pub hotspot_y: i32,
    pub rgba: Vec<u8>,
}

#[derive(Debug, Clone)]
pub struct CursorPositionData {
    pub visible: bool,
    /// Normalized 0..65535 within captured monitor.
    pub x: u16,
    pub y: u16,
}

pub enum CaptureFrameResult {
    /// New VRAM NV12 texture ready for zero-copy hardware encoding.
    Frame {
        nv12_texture: ID3D11Texture2D,
        capture_ms: f32,
        cursor_pos: Option<CursorPositionData>,
        cursor_shape: Option<CursorShapeData>,
    },
    /// Only pointer state changed (no desktop pixels changed).
    CursorOnly {
        cursor_pos: Option<CursorPositionData>,
        cursor_shape: Option<CursorShapeData>,
    },
    /// Timeout reached without desktop changes.
    Timeout,
    /// Duplication was lost (resolution change / UAC / lock screen) and reinitialized.
    Reinitialized,
}

/// Enumerate physical monitors and primary GPU adapter name via DXGI.
pub fn enumerate_displays_and_gpu() -> Result<(String, Vec<DisplayInfo>)> {
    unsafe {
        let factory: IDXGIFactory1 = CreateDXGIFactory1().context("CreateDXGIFactory1 failed")?;
        let mut displays = Vec::new();
        let mut primary_gpu = String::from("Unknown GPU");
        let mut global_idx = 0u32;

        let mut adapter_idx = 0u32;
        while let Ok(adapter) = factory.EnumAdapters1(adapter_idx) {
            adapter_idx += 1;
            let desc = adapter.GetDesc1()?;
            let gpu_name = utf16_null_to_string(&desc.Description);

            let mut output_idx = 0u32;
            while let Ok(output) = adapter.EnumOutputs(output_idx) {
                output_idx += 1;
                let odesc = output.GetDesc()?;
                if odesc.AttachedToDesktop.as_bool() {
                    if primary_gpu == "Unknown GPU" {
                        primary_gpu = gpu_name.clone();
                    }
                    let rect = odesc.DesktopCoordinates;
                    let width = (rect.right - rect.left).max(0) as u32;
                    let height = (rect.bottom - rect.top).max(0) as u32;
                    let is_primary = rect.left == 0 && rect.top == 0;
                    displays.push(DisplayInfo {
                        index: global_idx,
                        name: utf16_null_to_string(&odesc.DeviceName),
                        width,
                        height,
                        left: rect.left,
                        top: rect.top,
                        is_primary,
                    });
                    global_idx += 1;
                }
            }
        }

        Ok((primary_gpu, displays))
    }
}

/// Zero-Copy DXGI Desktop Duplication + GPU BGRA->NV12 color space converter.
pub struct DxgiZeroCopyCapturer {
    device: ID3D11Device,
    context: ID3D11DeviceContext,
    video_device: ID3D11VideoDevice,
    video_context: ID3D11VideoContext,
    video_enum: ID3D11VideoProcessorEnumerator,
    video_proc: ID3D11VideoProcessor,
    output1: IDXGIOutput1,
    duplication: IDXGIOutputDuplication,
    nv12_ring: Vec<(ID3D11Texture2D, ID3D11VideoProcessorOutputView)>,
    ring_idx: usize,
    src_width: u32,
    src_height: u32,
    out_width: u32,
    out_height: u32,
    last_nv12_valid: bool,
    pointer_buf: Vec<u8>,
}

impl DxgiZeroCopyCapturer {
    /// Initialize DXGI Desktop Duplication for `display_index` and allocate the GPU NV12 pipeline.
    /// If `max_width > 0` and source width exceeds it, GPU VideoProcessor scales down in the same pass.
    pub fn new(display_index: u32, max_width: u32) -> Result<Self> {
        unsafe {
            let (adapter, output1, src_width, src_height) =
                find_adapter_and_output(display_index)?;

            let mut device: Option<ID3D11Device> = None;
            let mut context: Option<ID3D11DeviceContext> = None;
            let feature_levels = [D3D_FEATURE_LEVEL_11_0];
            let flags = D3D11_CREATE_DEVICE_BGRA_SUPPORT | D3D11_CREATE_DEVICE_VIDEO_SUPPORT;

            D3D11CreateDevice(
                &adapter,
                D3D_DRIVER_TYPE_UNKNOWN,
                None,
                flags,
                Some(&feature_levels),
                D3D11_SDK_VERSION,
                Some(&mut device),
                None,
                Some(&mut context),
            )
            .context("D3D11CreateDevice failed")?;

            let device = device.ok_or_else(|| anyhow!("D3D11CreateDevice returned null device"))?;
            let context =
                context.ok_or_else(|| anyhow!("D3D11CreateDevice returned null context"))?;

            // Enable multithread protection because Media Foundation MFT shares this D3D11Device.
            let multithread: ID3D11Multithread = device.cast()?;
            let _ = multithread.SetMultithreadProtected(true);

            // Compute even output dimensions (required by NV12 / H.264 / HEVC).
            let (mut out_w, mut out_h) = if max_width > 0 && src_width > max_width {
                let scale = max_width as f64 / src_width as f64;
                (max_width, ((src_height as f64) * scale).round() as u32)
            } else {
                (src_width, src_height)
            };
            out_w = (out_w & !1).max(16);
            out_h = (out_h & !1).max(16);

            let video_device: ID3D11VideoDevice =
                device.cast().context("GPU does not expose ID3D11VideoDevice")?;
            let video_context: ID3D11VideoContext = context
                .cast()
                .context("GPU does not expose ID3D11VideoContext")?;

            let content_desc = D3D11_VIDEO_PROCESSOR_CONTENT_DESC {
                InputFrameFormat: D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE,
                InputFrameRate: DXGI_RATIONAL {
                    Numerator: 60,
                    Denominator: 1,
                },
                InputWidth: src_width,
                InputHeight: src_height,
                OutputFrameRate: DXGI_RATIONAL {
                    Numerator: 60,
                    Denominator: 1,
                },
                OutputWidth: out_w,
                OutputHeight: out_h,
                Usage: D3D11_VIDEO_USAGE_PLAYBACK_NORMAL,
            };

            let video_enum = video_device
                .CreateVideoProcessorEnumerator(&content_desc)
                .context("CreateVideoProcessorEnumerator failed")?;
            let video_proc = video_device
                .CreateVideoProcessor(&video_enum, 0)
                .context("CreateVideoProcessor failed")?;

            // Configure exact source and destination rects for optional GPU downscaling.
            let src_rect = RECT {
                left: 0,
                top: 0,
                right: src_width as i32,
                bottom: src_height as i32,
            };
            let dst_rect = RECT {
                left: 0,
                top: 0,
                right: out_w as i32,
                bottom: out_h as i32,
            };
            video_context.VideoProcessorSetStreamSourceRect(&video_proc, 0, true, Some(&src_rect));
            video_context.VideoProcessorSetStreamDestRect(&video_proc, 0, true, Some(&dst_rect));
            video_context.VideoProcessorSetOutputTargetRect(&video_proc, true, Some(&dst_rect));

            // Allocate a 2-texture NV12 ring buffer residing strictly in GPU VRAM.
            let mut nv12_ring = Vec::with_capacity(2);
            for _ in 0..2 {
                let tex_desc = D3D11_TEXTURE2D_DESC {
                    Width: out_w,
                    Height: out_h,
                    MipLevels: 1,
                    ArraySize: 1,
                    Format: DXGI_FORMAT_NV12,
                    SampleDesc: DXGI_SAMPLE_DESC {
                        Count: 1,
                        Quality: 0,
                    },
                    Usage: D3D11_USAGE_DEFAULT,
                    BindFlags: (D3D11_BIND_RENDER_TARGET.0 | D3D11_BIND_SHADER_RESOURCE.0) as u32,
                    CPUAccessFlags: 0, // Zero CPU access: strictly VRAM!
                    MiscFlags: 0,
                };
                let mut tex: Option<ID3D11Texture2D> = None;
                device
                    .CreateTexture2D(&tex_desc, None, Some(&mut tex))
                    .context("Failed to create NV12 VRAM texture")?;
                let tex = tex.unwrap();

                let out_view_desc = D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC {
                    ViewDimension: D3D11_VPOV_DIMENSION_TEXTURE2D,
                    Anonymous: D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC_0 {
                        Texture2D: D3D11_TEX2D_VPOV { MipSlice: 0 },
                    },
                };
                let mut out_view: Option<ID3D11VideoProcessorOutputView> = None;
                video_device
                    .CreateVideoProcessorOutputView(&tex, &video_enum, &out_view_desc, Some(&mut out_view))
                    .context("CreateVideoProcessorOutputView failed")?;

                nv12_ring.push((tex, out_view.unwrap()));
            }

            let duplication = output1
                .DuplicateOutput(&device)
                .context("IDXGIOutput1::DuplicateOutput failed")?;

            debug!(
                src_width,
                src_height, out_width = out_w, out_height = out_h,
                "DXGI Zero-Copy capturer initialized"
            );

            Ok(Self {
                device,
                context,
                video_device,
                video_context,
                video_enum,
                video_proc,
                output1,
                duplication,
                nv12_ring,
                ring_idx: 0,
                src_width,
                src_height,
                out_width: out_w,
                out_height: out_h,
                last_nv12_valid: false,
                pointer_buf: Vec::new(),
            })
        }
    }

    /// Expose the shared `ID3D11Device` so the Hardware Encoder binds to the exact same GPU device.
    pub fn d3d11_device(&self) -> &ID3D11Device {
        &self.device
    }

    pub fn output_dimensions(&self) -> (u32, u32) {
        (self.out_width, self.out_height)
    }

    pub fn source_dimensions(&self) -> (u32, u32) {
        (self.src_width, self.src_height)
    }

    /// Acquire the next frame directly from DWM in VRAM and convert BGRA8 -> NV12 on GPU.
    /// When `force_repeat_last` is true (e.g. IDR requested while desktop is static),
    /// returns the most recent NV12 VRAM texture immediately without waiting for screen changes.
    pub fn capture_next_frame(
        &mut self,
        timeout_ms: u32,
        force_repeat_last: bool,
    ) -> Result<CaptureFrameResult> {
        let t0 = Instant::now();
        let mut frame_info = DXGI_OUTDUPL_FRAME_INFO::default();
        let mut resource: Option<IDXGIResource> = None;

        let hr = unsafe {
            self.duplication
                .AcquireNextFrame(timeout_ms, &mut frame_info, &mut resource)
        };

        if let Err(err) = hr {
            if err.code() == DXGI_ERROR_WAIT_TIMEOUT {
                if force_repeat_last && self.last_nv12_valid {
                    let prev_idx = (self.ring_idx + self.nv12_ring.len() - 1) % self.nv12_ring.len();
                    return Ok(CaptureFrameResult::Frame {
                        nv12_texture: self.nv12_ring[prev_idx].0.clone(),
                        capture_ms: t0.elapsed().as_secs_f32() * 1000.0,
                        cursor_pos: None,
                        cursor_shape: None,
                    });
                }
                return Ok(CaptureFrameResult::Timeout);
            }
            if err.code() == DXGI_ERROR_ACCESS_LOST {
                warn!("DXGI_ERROR_ACCESS_LOST detected — recreating Desktop Duplication");
                self.recreate_duplication()?;
                return Ok(CaptureFrameResult::Reinitialized);
            }
            return Err(err.into());
        }

        // Always release the DXGI frame before returning!
        let result = self.process_acquired_frame(t0, &frame_info, resource, force_repeat_last);
        unsafe {
            let _ = self.duplication.ReleaseFrame();
        }
        result
    }

    fn process_acquired_frame(
        &mut self,
        t0: Instant,
        frame_info: &DXGI_OUTDUPL_FRAME_INFO,
        resource: Option<IDXGIResource>,
        force_repeat_last: bool,
    ) -> Result<CaptureFrameResult> {
        let cursor_pos = if frame_info.LastMouseUpdateTime != 0 {
            let x = if self.src_width > 1 {
                ((frame_info.PointerPosition.Position.x.max(0) as f64 / (self.src_width - 1) as f64)
                    * 65535.0)
                    .clamp(0.0, 65535.0) as u16
            } else {
                0
            };
            let y = if self.src_height > 1 {
                ((frame_info.PointerPosition.Position.y.max(0) as f64
                    / (self.src_height - 1) as f64)
                    * 65535.0)
                    .clamp(0.0, 65535.0) as u16
            } else {
                0
            };
            Some(CursorPositionData {
                visible: frame_info.PointerPosition.Visible.as_bool(),
                x,
                y,
            })
        } else {
            None
        };

        let cursor_shape = if frame_info.PointerShapeBufferSize > 0 {
            self.extract_cursor_shape(frame_info.PointerShapeBufferSize)
                .ok()
        } else {
            None
        };

        let has_desktop_pixels =
            frame_info.AccumulatedFrames > 0 || frame_info.LastPresentTime != 0;

        if !has_desktop_pixels && !force_repeat_last {
            return Ok(CaptureFrameResult::CursorOnly {
                cursor_pos,
                cursor_shape,
            });
        }

        let Some(res) = resource else {
            return Ok(CaptureFrameResult::CursorOnly {
                cursor_pos,
                cursor_shape,
            });
        };

        unsafe {
            let bgra_tex: ID3D11Texture2D = res.cast()?;
            let in_view_desc = D3D11_VIDEO_PROCESSOR_INPUT_VIEW_DESC {
                FourCC: 0,
                ViewDimension: D3D11_VPIV_DIMENSION_TEXTURE2D,
                Anonymous: D3D11_VIDEO_PROCESSOR_INPUT_VIEW_DESC_0 {
                    Texture2D: D3D11_TEX2D_VPIV {
                        MipSlice: 0,
                        ArraySlice: 0,
                    },
                },
            };
            let mut in_view: Option<ID3D11VideoProcessorInputView> = None;
            self.video_device
                .CreateVideoProcessorInputView(
                    &bgra_tex,
                    &self.video_enum,
                    &in_view_desc,
                    Some(&mut in_view),
                )
                .context("CreateVideoProcessorInputView failed")?;
            let in_view = in_view.unwrap();

            let (nv12_tex, nv12_out_view) = &self.nv12_ring[self.ring_idx];
            let mut streams = [D3D11_VIDEO_PROCESSOR_STREAM {
                Enable: true.into(),
                OutputIndex: 0,
                InputFrameOrField: 0,
                PastFrames: 0,
                FutureFrames: 0,
                ppPastSurfaces: std::ptr::null_mut(),
                pInputSurface: std::mem::ManuallyDrop::new(Some(in_view)),
                ppFutureSurfaces: std::ptr::null_mut(),
                ppPastSurfacesRight: std::ptr::null_mut(),
                pInputSurfaceRight: std::mem::ManuallyDrop::new(None),
                ppFutureSurfacesRight: std::ptr::null_mut(),
            }];

            let blt_res = self
                .video_context
                .VideoProcessorBlt(&self.video_proc, nv12_out_view, 0, &streams);

            // Properly drop the ManuallyDrop input view COM reference after Blt.
            std::mem::ManuallyDrop::drop(&mut streams[0].pInputSurface);
            std::mem::ManuallyDrop::drop(&mut streams[0].pInputSurfaceRight);

            blt_res.context("VideoProcessorBlt BGRA->NV12 failed")?;
            self.context.Flush();

            let out_tex = nv12_tex.clone();
            self.ring_idx = (self.ring_idx + 1) % self.nv12_ring.len();
            self.last_nv12_valid = true;

            Ok(CaptureFrameResult::Frame {
                nv12_texture: out_tex,
                capture_ms: t0.elapsed().as_secs_f32() * 1000.0,
                cursor_pos,
                cursor_shape,
            })
        }
    }

    fn extract_cursor_shape(&mut self, buf_size: u32) -> Result<CursorShapeData> {
        self.pointer_buf.resize(buf_size as usize, 0);
        let mut required = 0u32;
        let mut shape_info = DXGI_OUTDUPL_POINTER_SHAPE_INFO::default();
        unsafe {
            self.duplication.GetFramePointerShape(
                buf_size,
                self.pointer_buf.as_mut_ptr() as *mut _,
                &mut required,
                &mut shape_info,
            )?;
        }

        let w = shape_info.Width;
        let h = if shape_info.Type == DXGI_OUTDUPL_POINTER_SHAPE_TYPE_MONOCHROME.0 as u32 {
            shape_info.Height / 2
        } else {
            shape_info.Height
        };

        let mut rgba = vec![0u8; (w * h * 4) as usize];
        let shape_type = shape_info.Type;
        let pitch = shape_info.Pitch as usize;

        if shape_type == DXGI_OUTDUPL_POINTER_SHAPE_TYPE_COLOR.0 as u32
            || shape_type == DXGI_OUTDUPL_POINTER_SHAPE_TYPE_MASKED_COLOR.0 as u32
        {
            for y in 0..h as usize {
                for x in 0..w as usize {
                    let src = y * pitch + x * 4;
                    let dst = (y * w as usize + x) * 4;
                    if src + 4 <= self.pointer_buf.len() {
                        // BGRA -> RGBA
                        rgba[dst] = self.pointer_buf[src + 2];
                        rgba[dst + 1] = self.pointer_buf[src + 1];
                        rgba[dst + 2] = self.pointer_buf[src];
                        rgba[dst + 3] = if shape_type == DXGI_OUTDUPL_POINTER_SHAPE_TYPE_MASKED_COLOR.0 as u32 {
                            if self.pointer_buf[src + 3] == 0 { 255 } else { 0 }
                        } else {
                            self.pointer_buf[src + 3]
                        };
                    }
                }
            }
        } else if shape_type == DXGI_OUTDUPL_POINTER_SHAPE_TYPE_MONOCHROME.0 as u32 {
            for y in 0..h as usize {
                for x in 0..w as usize {
                    let byte_idx = x / 8;
                    let bit = 7 - (x % 8);
                    let and_row = y * pitch + byte_idx;
                    let xor_row = (y + h as usize) * pitch + byte_idx;
                    if xor_row < self.pointer_buf.len() {
                        let and_bit = (self.pointer_buf[and_row] >> bit) & 1;
                        let xor_bit = (self.pointer_buf[xor_row] >> bit) & 1;
                        let dst = (y * w as usize + x) * 4;
                        match (and_bit, xor_bit) {
                            (0, 0) => {
                                rgba[dst..dst + 4].copy_from_slice(&[0, 0, 0, 255]);
                            }
                            (0, 1) => {
                                rgba[dst..dst + 4].copy_from_slice(&[255, 255, 255, 255]);
                            }
                            (1, 1) => {
                                // Inverted cursor pixel -> render crisp white with slight border
                                rgba[dst..dst + 4].copy_from_slice(&[255, 255, 255, 220]);
                            }
                            _ => {
                                rgba[dst..dst + 4].copy_from_slice(&[0, 0, 0, 0]);
                            }
                        }
                    }
                }
            }
        }

        let hash = crc32fast::hash(&rgba) as u64 | ((w as u64) << 32) | ((h as u64) << 48);
        Ok(CursorShapeData {
            shape_id: hash,
            width: w,
            height: h,
            hotspot_x: shape_info.HotSpot.x,
            hotspot_y: shape_info.HotSpot.y,
            rgba,
        })
    }

    fn recreate_duplication(&mut self) -> Result<()> {
        unsafe {
            self.duplication = self
                .output1
                .DuplicateOutput(&self.device)
                .context("Failed to recreate IDXGIOutputDuplication")?;
        }
        Ok(())
    }
}

unsafe fn find_adapter_and_output(display_index: u32) -> Result<(IDXGIAdapter1, IDXGIOutput1, u32, u32)> {
    let factory: IDXGIFactory1 = CreateDXGIFactory1()?;
    let mut current_idx = 0u32;
    let mut adapter_idx = 0u32;

    loop {
        match factory.EnumAdapters1(adapter_idx) {
            Ok(adapter) => {
                adapter_idx += 1;
                let mut output_idx = 0u32;
                while let Ok(output) = adapter.EnumOutputs(output_idx) {
                    output_idx += 1;
                    let desc = output.GetDesc()?;
                    if !desc.AttachedToDesktop.as_bool() {
                        continue;
                    }
                    if current_idx == display_index {
                        let output1: IDXGIOutput1 = output.cast()?;
                        let rect = desc.DesktopCoordinates;
                        let w = (rect.right - rect.left).max(1) as u32;
                        let h = (rect.bottom - rect.top).max(1) as u32;
                        return Ok((adapter, output1, w, h));
                    }
                    current_idx += 1;
                }
            }
            Err(e) if e.code() == DXGI_ERROR_NOT_FOUND => break,
            Err(e) => return Err(e.into()),
        }
    }

    Err(anyhow!("Display index {display_index} not found"))
}

fn utf16_null_to_string(slice: &[u16]) -> String {
    let len = slice.iter().position(|&c| c == 0).unwrap_or(slice.len());
    String::from_utf16_lossy(&slice[..len])
}
