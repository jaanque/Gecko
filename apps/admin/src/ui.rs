use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::{Duration, Instant};

use eframe::egui::{
    self, Align, Color32, ColorImage, Rounding, FontId, Frame, Layout, Margin, PointerButton,
    Pos2, Rect, RichText, Sense, Stroke, TextureHandle, TextureOptions, Vec2,
};
use rmm_proto::{CriticalInputEvent, MouseButton, VideoCodec};

use crate::generator::{
    detect_local_ipv4, generate_configured_client, get_stub_status, reveal_in_explorer,
    GenerateClientRequest, GenerateClientResponse, StubStatus,
};
use crate::scancodes::{
    egui_key_to_scancode, SCANCODE_LALT, SCANCODE_LCTRL, SCANCODE_LSHIFT, SCANCODE_LWIN,
};
use crate::server::{
    adjust_encoder, request_idr, send_critical_input, send_mouse_move, start_remote_session,
    stop_remote_session,
};
use crate::state::{AdminState, AgentSummary};

// Gecko Obsidian + Emerald Palette
const BG_APP: Color32 = Color32::from_rgb(9, 11, 16);
const BG_SIDEBAR: Color32 = Color32::from_rgb(12, 15, 23);
const BG_CARD: Color32 = Color32::from_rgb(16, 19, 28);
const BG_CARD_HOVER: Color32 = Color32::from_rgb(20, 25, 38);
const BORDER_SUBTLE: Color32 = Color32::from_rgb(35, 41, 56);
const EMERALD: Color32 = Color32::from_rgb(16, 185, 129);
const EMERALD_SOFT: Color32 = Color32::from_rgb(52, 211, 153);
const SKY: Color32 = Color32::from_rgb(56, 189, 248);
const AMBER: Color32 = Color32::from_rgb(245, 158, 11);
const RED: Color32 = Color32::from_rgb(239, 68, 68);
const TEXT_PRIMARY: Color32 = Color32::from_rgb(244, 244, 248);
const TEXT_MUTED: Color32 = Color32::from_rgb(148, 157, 178);

#[derive(PartialEq, Eq)]
enum NavTab {
    Devices,
    Tokens,
}

struct ActiveViewerSession {
    machine_id: String,
    display_index: u32,
    target_fps: u32,
    bitrate_kbps: u32,
    input_enabled: bool,
    show_hud: bool,
    is_fullscreen: bool,
    epoch: u8,
    move_seq: u16,
    last_move_sent: Instant,
    last_sent_xy: (u16, u16),
    last_modifiers: egui::Modifiers,
    texture: Option<TextureHandle>,
    last_frame_version: u64,
    last_fps_tick: Instant,
    measured_fps: u32,
    error_banner: Option<String>,
}

struct GeneratorModalState {
    open: bool,
    label: String,
    admin_name: String,
    endpoints_text: String,
    file_name: String,
    ttl_hours: u64,
    error: Option<String>,
    result: Option<GenerateClientResponse>,
}

pub struct GeckoAdminApp {
    state: Arc<AdminState>,
    local_ips: Vec<String>,
    stub_status: StubStatus,
    active_tab: NavTab,
    search: String,
    copied_until: Option<Instant>,
    generator: GeneratorModalState,
    viewer: Option<ActiveViewerSession>,
}

impl GeckoAdminApp {
    pub fn new(cc: &eframe::CreationContext<'_>, state: Arc<AdminState>) -> Self {
        apply_gecko_theme(&cc.egui_ctx);
        state.set_ui_context(cc.egui_ctx.clone());

        let local_ips = detect_local_ipv4();
        let default_ep = format!(
            "{}:{}",
            local_ips.first().map(String::as_str).unwrap_or("127.0.0.1"),
            state.listen_port
        );
        let stub_status = get_stub_status();

        Self {
            state,
            local_ips,
            stub_status,
            active_tab: NavTab::Devices,
            search: String::new(),
            copied_until: None,
            generator: GeneratorModalState {
                open: false,
                label: "Soporte Express".into(),
                admin_name: "Soporte IT".into(),
                endpoints_text: default_ep,
                file_name: "Gecko-Soporte.exe".into(),
                ttl_hours: 24,
                error: None,
                result: None,
            },
            viewer: None,
        }
    }

    fn open_viewer_for(&mut self, agent: &AgentSummary) {
        // Close any previous active session first
        if let Some(prev) = self.viewer.take() {
            stop_remote_session(&self.state, &prev.machine_id);
        }

        let display_index = agent
            .displays
            .iter()
            .find(|d| d.is_primary)
            .map(|d| d.index)
            .unwrap_or(0);
        let target_fps = 60;
        let bitrate_kbps = 12_000;
        let epoch = self.state.next_input_epoch();

        // Enable input IMMEDIATELY on any remote PC; start paused with 1-click/F9 toggle ONLY
        // when testing against the exact same PC (prevents accidental self-mouse hijack loop).
        let input_enabled = !agent.is_same_pc;

        let err = start_remote_session(
            &self.state,
            &agent.machine_id,
            display_index,
            target_fps,
            bitrate_kbps,
            VideoCodec::H264,
        )
        .err()
        .map(|e| e.to_string());

        self.viewer = Some(ActiveViewerSession {
            machine_id: agent.machine_id.clone(),
            display_index,
            target_fps,
            bitrate_kbps,
            input_enabled,
            show_hud: true,
            is_fullscreen: false,
            epoch,
            move_seq: 0,
            last_move_sent: Instant::now(),
            last_sent_xy: (u16::MAX, u16::MAX),
            last_modifiers: egui::Modifiers::NONE,
            texture: None,
            last_frame_version: 0,
            last_fps_tick: Instant::now(),
            measured_fps: 0,
            error_banner: err,
        });
    }

    fn close_viewer(&mut self, ctx: &egui::Context) {
        if let Some(v) = self.viewer.take() {
            if v.is_fullscreen {
                ctx.send_viewport_cmd(egui::ViewportCommand::Fullscreen(false));
            }
            stop_remote_session(&self.state, &v.machine_id);
        }
    }
}

impl eframe::App for GeckoAdminApp {
    fn update(&mut self, ctx: &egui::Context, _frame: &mut eframe::Frame) {
        // If a remote viewer is open, render the full-window native remote viewer!
        if self.viewer.is_some() {
            self.render_remote_viewer(ctx);
            return;
        }

        // Refresh periodically for online RTT / devices list
        ctx.request_repaint_after(Duration::from_millis(500));

        self.render_sidebar(ctx);
        self.render_main_dashboard(ctx);

        if self.generator.open {
            self.render_generator_modal(ctx);
        }
    }

    fn on_exit(&mut self, _gl: Option<&eframe::glow::Context>) {
        if let Some(v) = self.viewer.take() {
            stop_remote_session(&self.state, &v.machine_id);
        }
    }
}

impl GeckoAdminApp {
    fn render_sidebar(&mut self, ctx: &egui::Context) {
        egui::SidePanel::left("gecko_sidebar")
            .exact_width(265.0)
            .resizable(false)
            .frame(
                Frame::none()
                    .fill(BG_SIDEBAR)
                    .inner_margin(Margin::same(16.0))
                    .stroke(Stroke::new(1.0_f32, BORDER_SUBTLE)),
            )
            .show(ctx, |ui| {
                // Brand Header
                ui.horizontal(|ui| {
                    let (rect, _) = ui.allocate_exact_size(Vec2::splat(36.0), Sense::hover());
                    ui.painter()
                        .rect_filled(rect, Rounding::same(10.0), EMERALD);
                    ui.painter().text(
                        rect.center(),
                        egui::Align2::CENTER_CENTER,
                        "G",
                        FontId::proportional(20.0),
                        Color32::from_rgb(9, 11, 16),
                    );
                    ui.add_space(6.0);
                    ui.vertical(|ui| {
                        ui.horizontal(|ui| {
                            ui.label(
                                RichText::new("Gecko")
                                    .size(17.0)
                                    .strong()
                                    .color(TEXT_PRIMARY),
                            );
                            Frame::none()
                                .fill(Color32::from_rgba_unmultiplied(16, 185, 129, 32))
                                .rounding(4.0)
                                .inner_margin(Margin::symmetric(5.0, 1.0))
                                .show(ui, |ui| {
                                    ui.label(
                                        RichText::new("NATIVO")
                                            .size(9.5)
                                            .monospace()
                                            .strong()
                                            .color(EMERALD_SOFT),
                                    );
                                });
                        });
                        ui.label(
                            RichText::new("Reverse Zero-Copy Desktop")
                                .size(11.0)
                                .color(TEXT_MUTED),
                        );
                    });
                });

                ui.add_space(18.0);

                // Primary CTA Button: Generar Cliente
                let gen_btn = egui::Button::new(
                    RichText::new("  +  Generar Cliente (.EXE)")
                        .size(13.0)
                        .strong()
                        .color(Color32::from_rgb(9, 11, 16)),
                )
                .fill(EMERALD)
                .rounding(10.0)
                .min_size(Vec2::new(ui.available_width(), 38.0));

                if ui.add(gen_btn).clicked() {
                    self.stub_status = get_stub_status();
                    self.local_ips = detect_local_ipv4();
                    self.generator.result = None;
                    self.generator.error = None;
                    self.generator.open = true;
                }

                ui.add_space(16.0);

                // Navigation Tabs
                let online_count = self.state.agents.read().len();
                let token_count = self.state.tokens.read().len();

                self.nav_button(
                    ui,
                    "Equipos",
                    &online_count.to_string(),
                    self.active_tab == NavTab::Devices,
                    EMERALD_SOFT,
                    || NavTab::Devices,
                );
                ui.add_space(4.0);
                self.nav_button(
                    ui,
                    "Clientes Emitidos",
                    &token_count.to_string(),
                    self.active_tab == NavTab::Tokens,
                    SKY,
                    || NavTab::Tokens,
                );

                // Push server status card to bottom
                ui.with_layout(Layout::bottom_up(Align::LEFT), |ui| {
                    Frame::none()
                        .fill(BG_APP)
                        .stroke(Stroke::new(1.0_f32, BORDER_SUBTLE))
                        .rounding(12.0)
                        .inner_margin(Margin::same(12.0))
                        .show(ui, |ui| {
                            ui.set_width(ui.available_width());
                            ui.horizontal(|ui| {
                                ui.label(
                                    RichText::new("●")
                                        .color(EMERALD)
                                        .size(11.0),
                                );
                                ui.label(
                                    RichText::new("Backend QUIC Activo")
                                        .size(11.5)
                                        .strong()
                                        .color(TEXT_PRIMARY),
                                );
                            });
                            ui.add_space(6.0);
                            info_row(
                                ui,
                                "QUIC UDP",
                                &format!(":{}", self.state.listen_port),
                                EMERALD_SOFT,
                            );
                            info_row(
                                ui,
                                "Auto-LAN",
                                &format!("UDP :{}", self.state.discovery_port),
                                SKY,
                            );
                            info_row(
                                ui,
                                "IP Local",
                                self.local_ips.first().map(String::as_str).unwrap_or("—"),
                                TEXT_PRIMARY,
                            );
                            info_row(
                                ui,
                                "Plantilla",
                                if self.stub_status.found {
                                    "Lista (Stub OK)"
                                } else {
                                    "Sin compilar"
                                },
                                if self.stub_status.found {
                                    EMERALD_SOFT
                                } else {
                                    AMBER
                                },
                            );

                            ui.add_space(6.0);
                            let fp = self.state.identity.cert_fingerprint_hex();
                            let copied = self
                                .copied_until
                                .is_some_and(|t| Instant::now() < t);
                            let fp_label = if copied {
                                "Huella TLS copiada".to_string()
                            } else {
                                format!("TLS: {}… (copiar)", &fp[..14])
                            };
                            if ui
                                .add(
                                    egui::Button::new(
                                        RichText::new(fp_label)
                                            .monospace()
                                            .size(10.5)
                                            .color(if copied { EMERALD_SOFT } else { TEXT_MUTED }),
                                    )
                                    .fill(BG_CARD)
                                    .stroke(Stroke::new(1.0_f32, BORDER_SUBTLE))
                                    .rounding(8.0)
                                    .min_size(Vec2::new(ui.available_width(), 26.0)),
                                )
                                .clicked()
                            {
                                ui.ctx().copy_text(fp);
                                self.copied_until = Some(Instant::now() + Duration::from_secs(2));
                            }
                        });
                });
            });
    }

    fn nav_button(
        &mut self,
        ui: &mut egui::Ui,
        title: &str,
        badge: &str,
        selected: bool,
        badge_color: Color32,
        target: impl FnOnce() -> NavTab,
    ) {
        let fill = if selected {
            Color32::from_rgb(26, 32, 46)
        } else {
            Color32::TRANSPARENT
        };
        let resp = Frame::none()
            .fill(fill)
            .rounding(10.0)
            .inner_margin(Margin::symmetric(12.0, 9.0))
            .show(ui, |ui| {
                ui.set_width(ui.available_width());
                ui.horizontal(|ui| {
                    ui.label(
                        RichText::new(title)
                            .size(13.0)
                            .strong()
                            .color(if selected { TEXT_PRIMARY } else { TEXT_MUTED }),
                    );
                    ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                        Frame::none()
                            .fill(Color32::from_rgba_unmultiplied(
                                badge_color.r(),
                                badge_color.g(),
                                badge_color.b(),
                                30,
                            ))
                            .rounding(6.0)
                            .inner_margin(Margin::symmetric(7.0, 2.0))
                            .show(ui, |ui| {
                                ui.label(
                                    RichText::new(badge)
                                        .monospace()
                                        .size(11.0)
                                        .color(badge_color),
                                );
                            });
                    });
                });
            })
            .response
            .interact(Sense::click());

        if resp.clicked() {
            self.active_tab = target();
        }
    }

    fn render_main_dashboard(&mut self, ctx: &egui::Context) {
        egui::CentralPanel::default()
            .frame(Frame::none().fill(BG_APP).inner_margin(Margin::same(24.0)))
            .show(ctx, |ui| {
                // Header
                ui.horizontal(|ui| {
                    ui.vertical(|ui| {
                        let heading = match self.active_tab {
                            NavTab::Devices => "Equipos Remotos (Conexión Inversa)",
                            NavTab::Tokens => "Clientes Emitidos y Revocación",
                        };
                        ui.label(
                            RichText::new(heading)
                                .size(19.0)
                                .strong()
                                .color(TEXT_PRIMARY),
                        );
                        let subtitle = match self.active_tab {
                            NavTab::Devices => {
                                "Haz doble clic o pulsa Controlar en cualquier equipo Online para iniciar sesión Zero-Copy."
                            }
                            NavTab::Tokens => {
                                "Cada ejecutable lleva un token Ed25519 único y revocable al instante."
                            }
                        };
                        ui.label(RichText::new(subtitle).size(12.0).color(TEXT_MUTED));
                    });

                    if self.active_tab == NavTab::Devices {
                        ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                            ui.add(
                                egui::TextEdit::singleline(&mut self.search)
                                    .hint_text("Buscar PC, usuario o IP...")
                                    .desired_width(240.0)
                                    .margin(Margin::symmetric(10.0, 7.0)),
                            );
                        });
                    }
                });

                ui.add_space(16.0);
                ui.separator();
                ui.add_space(16.0);

                match self.active_tab {
                    NavTab::Devices => self.render_devices_view(ui),
                    NavTab::Tokens => self.render_tokens_view(ui),
                }
            });
    }

    fn render_devices_view(&mut self, ui: &mut egui::Ui) {
        let agents = self.state.agent_summaries();
        let offline = self.state.offline_devices();
        let query = self.search.trim().to_lowercase();

        let filtered_online: Vec<AgentSummary> = agents
            .into_iter()
            .filter(|a| {
                query.is_empty()
                    || a.hostname.to_lowercase().contains(&query)
                    || a.os_user.to_lowercase().contains(&query)
                    || a.label.to_lowercase().contains(&query)
                    || a.remote_addr.to_lowercase().contains(&query)
            })
            .collect();

        let filtered_offline: Vec<_> = offline
            .into_iter()
            .filter(|d| {
                query.is_empty()
                    || d.hostname.to_lowercase().contains(&query)
                    || d.os_user.to_lowercase().contains(&query)
                    || d.label.to_lowercase().contains(&query)
            })
            .collect();

        if filtered_online.is_empty() && filtered_offline.is_empty() {
            ui.centered_and_justified(|ui| {
                ui.vertical_centered(|ui| {
                    ui.add_space(60.0);
                    ui.label(
                        RichText::new(format!(
                            "Esperando conexiones en UDP :{} (Auto-Discovery LAN :{})",
                            self.state.listen_port, self.state.discovery_port
                        ))
                        .size(16.0)
                        .strong()
                        .color(TEXT_PRIMARY),
                    );
                    ui.add_space(8.0);
                    ui.label(
                        RichText::new(
                            "Pulsa «Generar Cliente (.EXE)» en la barra izquierda.\nEn cuanto el usuario haga doble clic en el .exe, su equipo aparecerá aquí automáticamente.",
                        )
                        .size(12.5)
                        .color(TEXT_MUTED),
                    );
                    ui.add_space(18.0);
                    if ui
                        .add(
                            egui::Button::new(
                                RichText::new("  +  Generar primer cliente")
                                    .size(13.0)
                                    .strong()
                                    .color(EMERALD_SOFT),
                            )
                            .fill(Color32::from_rgba_unmultiplied(16, 185, 129, 28))
                            .stroke(Stroke::new(1.0_f32, EMERALD))
                            .rounding(10.0)
                            .min_size(Vec2::new(210.0, 38.0)),
                        )
                        .clicked()
                    {
                        self.stub_status = get_stub_status();
                        self.generator.result = None;
                        self.generator.error = None;
                        self.generator.open = true;
                    }
                });
            });
            return;
        }

        let mut connect_target: Option<AgentSummary> = None;
        let mut disconnect_target: Option<String> = None;

        egui::ScrollArea::vertical().show(ui, |ui| {
            if !filtered_online.is_empty() {
                ui.label(
                    RichText::new(format!("ONLINE ({})", filtered_online.len()))
                        .size(11.0)
                        .strong()
                        .color(EMERALD_SOFT),
                );
                ui.add_space(8.0);

                for agent in &filtered_online {
                    let card_frame = Frame::none()
                        .fill(BG_CARD)
                        .stroke(Stroke::new(1.0_f32, BORDER_SUBTLE))
                        .rounding(14.0)
                        .inner_margin(Margin::same(16.0));

                    let resp = card_frame
                        .show(ui, |ui| {
                            ui.set_width(ui.available_width());
                            ui.horizontal(|ui| {
                                // Left: Hostname, user, label, GPU
                                ui.vertical(|ui| {
                                    ui.horizontal(|ui| {
                                        ui.label(RichText::new("●").color(EMERALD).size(12.0));
                                        ui.label(
                                            RichText::new(&agent.hostname)
                                                .size(15.5)
                                                .strong()
                                                .color(TEXT_PRIMARY),
                                        );
                                        if agent.is_same_pc {
                                            Frame::none()
                                                .fill(Color32::from_rgba_unmultiplied(245, 158, 11, 30))
                                                .rounding(6.0)
                                                .inner_margin(Margin::symmetric(6.0, 2.0))
                                                .show(ui, |ui| {
                                                    ui.label(
                                                        RichText::new("ESTE PC")
                                                            .size(10.0)
                                                            .strong()
                                                            .color(AMBER),
                                                    );
                                                });
                                        }
                                        Frame::none()
                                            .fill(Color32::from_rgba_unmultiplied(16, 185, 129, 25))
                                            .rounding(6.0)
                                            .inner_margin(Margin::symmetric(6.0, 2.0))
                                            .show(ui, |ui| {
                                                ui.label(
                                                    RichText::new(&agent.label)
                                                        .size(10.5)
                                                        .color(EMERALD_SOFT),
                                                );
                                            });
                                    });

                                    ui.add_space(4.0);
                                    ui.horizontal(|ui| {
                                        ui.label(
                                            RichText::new(format!(
                                                "Usuario: {}  •  GPU: {}  •  {} monitor(es)",
                                                agent.os_user,
                                                agent.gpu_name,
                                                agent.displays.len()
                                            ))
                                            .size(11.5)
                                            .color(TEXT_MUTED),
                                        );
                                    });
                                    ui.label(
                                        RichText::new(format!(
                                            "IP: {}  •  RTT: {} ms  •  ID: {}",
                                            agent.remote_addr,
                                            agent.rtt_ms,
                                            &agent.machine_id[..10]
                                        ))
                                        .monospace()
                                        .size(11.0)
                                        .color(Color32::from_rgb(120, 132, 156)),
                                    );
                                });

                                // Right: Actions
                                ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                                    if ui
                                        .add(
                                            egui::Button::new(
                                                RichText::new("Desconectar")
                                                    .size(12.0)
                                                    .color(RED),
                                            )
                                            .fill(Color32::from_rgba_unmultiplied(239, 68, 68, 22))
                                            .stroke(Stroke::new(1.0_f32, Color32::from_rgb(120, 35, 42)))
                                            .rounding(9.0)
                                            .min_size(Vec2::new(98.0, 34.0)),
                                        )
                                        .clicked()
                                    {
                                        disconnect_target = Some(agent.machine_id.clone());
                                    }

                                    ui.add_space(6.0);

                                    if ui
                                        .add(
                                            egui::Button::new(
                                                RichText::new("▶  Controlar")
                                                    .size(12.5)
                                                    .strong()
                                                    .color(Color32::from_rgb(9, 11, 16)),
                                            )
                                            .fill(EMERALD)
                                            .rounding(9.0)
                                            .min_size(Vec2::new(118.0, 34.0)),
                                        )
                                        .clicked()
                                    {
                                        connect_target = Some(agent.clone());
                                    }
                                });
                            });
                        })
                        .response
                        .interact(Sense::click());

                    if resp.double_clicked() {
                        connect_target = Some(agent.clone());
                    }

                    ui.add_space(10.0);
                }
            }

            if !filtered_offline.is_empty() {
                ui.add_space(12.0);
                ui.label(
                    RichText::new(format!(
                        "RECONOCIDOS OFFLINE ({})",
                        filtered_offline.len()
                    ))
                    .size(11.0)
                    .strong()
                    .color(TEXT_MUTED),
                );
                ui.add_space(8.0);

                for dev in &filtered_offline {
                    Frame::none()
                        .fill(Color32::from_rgb(13, 15, 22))
                        .stroke(Stroke::new(1.0_f32, Color32::from_rgb(28, 32, 44)))
                        .rounding(12.0)
                        .inner_margin(Margin::symmetric(16.0, 12.0))
                        .show(ui, |ui| {
                            ui.set_width(ui.available_width());
                            ui.horizontal(|ui| {
                                ui.label(RichText::new("○").color(TEXT_MUTED));
                                ui.label(
                                    RichText::new(&dev.hostname)
                                        .size(13.5)
                                        .strong()
                                        .color(Color32::from_rgb(180, 186, 200)),
                                );
                                ui.label(
                                    RichText::new(format!(
                                        "({}) • {} • {}",
                                        dev.os_user, dev.label, dev.gpu_name
                                    ))
                                    .size(11.5)
                                    .color(TEXT_MUTED),
                                );
                                ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                                    ui.label(
                                        RichText::new(format!(
                                            "Última vez: {}",
                                            dev.last_seen.format("%d/%m %H:%M")
                                        ))
                                        .monospace()
                                        .size(11.0)
                                        .color(TEXT_MUTED),
                                    );
                                });
                            });
                        });
                    ui.add_space(6.0);
                }
            }
        });

        if let Some(mid) = disconnect_target {
            self.state.disconnect_agent(&mid);
        }
        if let Some(agent) = connect_target {
            self.open_viewer_for(&agent);
        }
    }

    fn render_tokens_view(&mut self, ui: &mut egui::Ui) {
        let tokens = self.state.tokens.read().clone();
        if tokens.is_empty() {
            ui.centered_and_justified(|ui| {
                ui.label(
                    RichText::new("Aún no has generado ningún ejecutable cliente.")
                        .size(13.0)
                        .color(TEXT_MUTED),
                );
            });
            return;
        }

        let mut revoke_id: Option<String> = None;

        egui::ScrollArea::vertical().show(ui, |ui| {
            for t in &tokens {
                Frame::none()
                    .fill(BG_CARD)
                    .stroke(Stroke::new(1.0_f32, BORDER_SUBTLE))
                    .rounding(12.0)
                    .inner_margin(Margin::same(14.0))
                    .show(ui, |ui| {
                        ui.set_width(ui.available_width());
                        ui.horizontal(|ui| {
                            ui.vertical(|ui| {
                                ui.horizontal(|ui| {
                                    ui.label(
                                        RichText::new(&t.label)
                                            .size(14.0)
                                            .strong()
                                            .color(TEXT_PRIMARY),
                                    );
                                    let (status_txt, status_col) = if t.revoked {
                                        ("REVOCADO", RED)
                                    } else {
                                        ("ACTIVO", EMERALD_SOFT)
                                    };
                                    Frame::none()
                                        .fill(Color32::from_rgba_unmultiplied(
                                            status_col.r(),
                                            status_col.g(),
                                            status_col.b(),
                                            28,
                                        ))
                                        .rounding(6.0)
                                        .inner_margin(Margin::symmetric(6.0, 2.0))
                                        .show(ui, |ui| {
                                            ui.label(
                                                RichText::new(status_txt)
                                                    .size(10.0)
                                                    .strong()
                                                    .color(status_col),
                                            );
                                        });
                                });
                                ui.label(
                                    RichText::new(format!(
                                        "Endpoints: {}  •  Token: {}",
                                        t.endpoints.join(", "),
                                        &t.token_id[..12]
                                    ))
                                    .monospace()
                                    .size(11.0)
                                    .color(TEXT_MUTED),
                                );
                            });

                            ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                                if !t.revoked
                                    && ui
                                        .add(
                                            egui::Button::new(
                                                RichText::new("Revocar").size(11.5).color(RED),
                                            )
                                            .fill(Color32::from_rgba_unmultiplied(239, 68, 68, 22))
                                            .rounding(8.0),
                                        )
                                        .clicked()
                                {
                                    revoke_id = Some(t.token_id.clone());
                                }
                                if ui
                                    .add(
                                        egui::Button::new(
                                            RichText::new("Abrir Archivo")
                                                .size(11.5)
                                                .color(TEXT_PRIMARY),
                                        )
                                        .fill(BG_CARD_HOVER)
                                        .rounding(8.0),
                                    )
                                    .clicked()
                                {
                                    reveal_in_explorer(&t.output_path);
                                }
                            });
                        });
                    });
                ui.add_space(8.0);
            }
        });

        if let Some(tid) = revoke_id {
            let _ = self.state.revoke_token(&tid);
        }
    }

    fn render_generator_modal(&mut self, ctx: &egui::Context) {
        let mut open = self.generator.open;
        egui::Window::new("Generar Cliente Zero-Config (Gecko)")
            .open(&mut open)
            .collapsible(false)
            .resizable(false)
            .anchor(egui::Align2::CENTER_CENTER, Vec2::ZERO)
            .fixed_size(Vec2::new(510.0, 420.0))
            .frame(
                Frame::none()
                    .fill(BG_CARD)
                    .stroke(Stroke::new(1.0_f32, BORDER_SUBTLE))
                    .rounding(16.0)
                    .inner_margin(Margin::same(20.0)),
            )
            .show(ctx, |ui| {
                if !self.stub_status.found {
                    Frame::none()
                        .fill(Color32::from_rgba_unmultiplied(245, 158, 11, 25))
                        .stroke(Stroke::new(1.0_f32, AMBER))
                        .rounding(10.0)
                        .inner_margin(Margin::same(10.0))
                        .show(ui, |ui| {
                            ui.label(
                                RichText::new(
                                    "Plantilla rmm-agent.exe no encontrada. Compila primero con:\ncargo build -p rmm-agent --profile release-agent",
                                )
                                .size(11.5)
                                .color(AMBER),
                            );
                        });
                    ui.add_space(10.0);
                }

                if let Some(ref res) = self.generator.result.clone() {
                    Frame::none()
                        .fill(Color32::from_rgba_unmultiplied(16, 185, 129, 25))
                        .stroke(Stroke::new(1.0_f32, EMERALD))
                        .rounding(12.0)
                        .inner_margin(Margin::same(14.0))
                        .show(ui, |ui| {
                            ui.label(
                                RichText::new("✓ Ejecutable cliente generado correctamente")
                                    .size(14.0)
                                    .strong()
                                    .color(EMERALD_SOFT),
                            );
                            ui.add_space(6.0);
                            ui.label(
                                RichText::new(
                                    "El usuario solo debe hacer doble clic: sin IDs, sin contraseñas y con auto-descubrimiento LAN.",
                                )
                                .size(11.5)
                                .color(TEXT_PRIMARY),
                            );
                            ui.add_space(8.0);
                            ui.label(
                                RichText::new(&res.output_path)
                                    .monospace()
                                    .size(11.0)
                                    .color(SKY),
                            );
                            ui.add_space(4.0);
                            ui.label(
                                RichText::new(format!(
                                    "Tamaño: {:.2} MB  •  Token: {}",
                                    res.size_bytes as f64 / (1024.0 * 1024.0),
                                    &res.token_id[..8]
                                ))
                                .monospace()
                                .size(11.0)
                                .color(TEXT_MUTED),
                            );
                        });

                    ui.add_space(16.0);
                    ui.horizontal(|ui| {
                        if ui
                            .add(
                                egui::Button::new(
                                    RichText::new("Abrir en Explorador")
                                        .strong()
                                        .color(Color32::from_rgb(9, 11, 16)),
                                )
                                .fill(EMERALD)
                                .rounding(8.0)
                                .min_size(Vec2::new(160.0, 34.0)),
                            )
                            .clicked()
                        {
                            reveal_in_explorer(&res.output_path);
                        }
                        if ui
                            .add(
                                egui::Button::new("Generar otro")
                                    .fill(BG_CARD_HOVER)
                                    .rounding(8.0)
                                    .min_size(Vec2::new(110.0, 34.0)),
                            )
                            .clicked()
                        {
                            self.generator.result = None;
                        }
                    });
                    return;
                }

                ui.label(RichText::new("Etiqueta del Cliente").size(12.0).color(TEXT_MUTED));
                if ui
                    .add(
                        egui::TextEdit::singleline(&mut self.generator.label)
                            .desired_width(f32::INFINITY),
                    )
                    .changed()
                {
                    let clean: String = self
                        .generator
                        .label
                        .chars()
                        .map(|c| if c.is_ascii_alphanumeric() { c } else { '-' })
                        .collect();
                    let trimmed = clean.trim_matches('-');
                    self.generator.file_name = format!(
                        "Gecko-{}.exe",
                        if trimmed.is_empty() { "Cliente" } else { trimmed }
                    );
                }

                ui.add_space(8.0);
                ui.label(
                    RichText::new("Nombre del Administrador (visible en bandeja)")
                        .size(12.0)
                        .color(TEXT_MUTED),
                );
                ui.add(
                    egui::TextEdit::singleline(&mut self.generator.admin_name)
                        .desired_width(f32::INFINITY),
                );

                ui.add_space(8.0);
                ui.label(
                    RichText::new("Endpoints del Admin (IP:Puerto UDP, 1 por línea)")
                        .size(12.0)
                        .color(TEXT_MUTED),
                );
                ui.add(
                    egui::TextEdit::multiline(&mut self.generator.endpoints_text)
                        .desired_rows(2)
                        .desired_width(f32::INFINITY)
                        .font(egui::TextStyle::Monospace),
                );
                ui.horizontal(|ui| {
                    for ip in &self.local_ips {
                        let ep = format!("{}:{}", ip, self.state.listen_port);
                        if ui
                            .small_button(RichText::new(format!("+ {ep}")).monospace().size(10.5))
                            .clicked()
                            && !self.generator.endpoints_text.contains(&ep)
                        {
                            if !self.generator.endpoints_text.trim().is_empty() {
                                self.generator.endpoints_text.push('\n');
                            }
                            self.generator.endpoints_text.push_str(&ep);
                        }
                    }
                });

                ui.add_space(8.0);
                ui.horizontal(|ui| {
                    ui.vertical(|ui| {
                        ui.label(RichText::new("Nombre del .EXE").size(12.0).color(TEXT_MUTED));
                        ui.add(
                            egui::TextEdit::singleline(&mut self.generator.file_name)
                                .desired_width(250.0)
                                .font(egui::TextStyle::Monospace),
                        );
                    });
                    ui.add_space(12.0);
                    ui.vertical(|ui| {
                        ui.label(RichText::new("Caducidad").size(12.0).color(TEXT_MUTED));
                        egui::ComboBox::from_id_salt("ttl_combo")
                            .selected_text(match self.generator.ttl_hours {
                                1 => "1 hora",
                                24 => "24 horas",
                                168 => "7 días",
                                _ => "Sin caducidad",
                            })
                            .show_ui(ui, |ui| {
                                ui.selectable_value(&mut self.generator.ttl_hours, 1, "1 hora");
                                ui.selectable_value(&mut self.generator.ttl_hours, 24, "24 horas");
                                ui.selectable_value(&mut self.generator.ttl_hours, 168, "7 días");
                                ui.selectable_value(&mut self.generator.ttl_hours, 0, "Sin caducidad");
                            });
                    });
                });

                if let Some(ref err) = self.generator.error {
                    ui.add_space(8.0);
                    ui.label(RichText::new(err).size(11.5).color(RED));
                }

                ui.add_space(16.0);
                ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                    let submit = egui::Button::new(
                        RichText::new("  Generar .EXE  ")
                            .strong()
                            .color(Color32::from_rgb(9, 11, 16)),
                    )
                    .fill(EMERALD)
                    .rounding(8.0)
                    .min_size(Vec2::new(135.0, 34.0));

                    if ui.add_enabled(self.stub_status.found, submit).clicked() {
                        let endpoints: Vec<String> = self
                            .generator
                            .endpoints_text
                            .lines()
                            .flat_map(|l| l.split(','))
                            .map(|s| s.trim().to_string())
                            .filter(|s| !s.is_empty())
                            .collect();

                        let req = GenerateClientRequest {
                            label: self.generator.label.clone(),
                            admin_name: self.generator.admin_name.clone(),
                            endpoints,
                            ttl_hours: if self.generator.ttl_hours == 0 {
                                None
                            } else {
                                Some(self.generator.ttl_hours)
                            },
                            file_name: Some(self.generator.file_name.clone()),
                        };

                        match generate_configured_client(&self.state, req) {
                            Ok(res) => {
                                self.generator.error = None;
                                self.generator.result = Some(res);
                            }
                            Err(e) => {
                                self.generator.error = Some(e.to_string());
                            }
                        }
                    }
                });
            });

        self.generator.open = open;
    }

    fn render_remote_viewer(&mut self, ctx: &egui::Context) {
        let machine_id = self.viewer.as_ref().unwrap().machine_id.clone();
        let agent_opt = self.state.agents.read().get(&machine_id).map(|a| a.summary.clone());

        let Some(agent) = agent_opt else {
            self.close_viewer(ctx);
            return;
        };

        // Measure local rendered FPS every second
        {
            let v = self.viewer.as_mut().unwrap();
            if v.last_fps_tick.elapsed() >= Duration::from_secs(1) {
                v.measured_fps = self
                    .state
                    .viewer_frame
                    .rendered_frames
                    .swap(0, Ordering::Relaxed) as u32;
                v.last_fps_tick = Instant::now();
            }
        }

        // Toggle input with F9 (super handy for local testing & quick pause!)
        let f9_pressed = ctx.input(|i| i.key_pressed(egui::Key::F9));
        if f9_pressed {
            let v = self.viewer.as_mut().unwrap();
            v.input_enabled = !v.input_enabled;
            if !v.input_enabled {
                send_critical_input(&self.state, &machine_id, CriticalInputEvent::ReleaseAll);
            }
        }

        // Upload newest decoded RGBA frame into GPU texture when version advances
        let cur_ver = self.state.viewer_frame.version.load(Ordering::Acquire);
        {
            let v = self.viewer.as_mut().unwrap();
            if cur_ver != v.last_frame_version {
                if let Some(frame) = self.state.viewer_frame.latest.lock().take() {
                    let img = ColorImage::from_rgba_unmultiplied(
                        [frame.width as usize, frame.height as usize],
                        &frame.rgba,
                    );
                    if let Some(ref mut tex) = v.texture {
                        tex.set(img, TextureOptions::LINEAR);
                    } else {
                        v.texture = Some(ctx.load_texture(
                            "gecko_remote_video",
                            img,
                            TextureOptions::LINEAR,
                        ));
                    }
                    v.last_frame_version = cur_ver;
                }
            }
        }

        let mut want_close = false;
        let mut switch_display: Option<u32> = None;

        // Top Control Bar (Native Toolbar — never overlaps/blocks the remote screen!)
        egui::TopBottomPanel::top("viewer_toolbar")
            .exact_height(46.0)
            .frame(
                Frame::none()
                    .fill(BG_SIDEBAR)
                    .stroke(Stroke::new(1.0_f32, BORDER_SUBTLE))
                    .inner_margin(Margin::symmetric(14.0, 6.0)),
            )
            .show(ctx, |ui| {
                let v = self.viewer.as_mut().unwrap();
                ui.horizontal_centered(|ui| {
                    ui.label(RichText::new("●").color(EMERALD).size(12.0));
                    ui.label(
                        RichText::new(&agent.hostname)
                            .size(13.5)
                            .strong()
                            .color(TEXT_PRIMARY),
                    );
                    ui.label(
                        RichText::new(format!("({})", agent.os_user))
                            .size(11.5)
                            .color(TEXT_MUTED),
                    );

                    ui.separator();

                    // Active HW encoder badge
                    let enc_name = agent
                        .encoder_name
                        .as_deref()
                        .unwrap_or("Inicializando encoder...");
                    let enc_col = if agent.is_hardware_encoder {
                        EMERALD_SOFT
                    } else {
                        AMBER
                    };
                    ui.label(RichText::new(enc_name).monospace().size(11.0).color(enc_col));

                    // Multi-monitor selector
                    if agent.displays.len() > 1 {
                        ui.separator();
                        for d in &agent.displays {
                            let active = v.display_index == d.index;
                            let btn = egui::Button::new(
                                RichText::new(format!("#{}", d.index + 1))
                                    .monospace()
                                    .size(11.0)
                                    .color(if active {
                                        Color32::from_rgb(9, 11, 16)
                                    } else {
                                        TEXT_PRIMARY
                                    }),
                            )
                            .fill(if active { EMERALD } else { BG_CARD })
                            .rounding(6.0);
                            if ui.add(btn).clicked() && !active {
                                switch_display = Some(d.index);
                            }
                        }
                    }

                    ui.separator();

                    // Bitrate selector
                    let prev_br = v.bitrate_kbps;
                    egui::ComboBox::from_id_salt("viewer_bitrate")
                        .width(84.0)
                        .selected_text(format!("{} Mbps", v.bitrate_kbps / 1000))
                        .show_ui(ui, |ui| {
                            ui.selectable_value(&mut v.bitrate_kbps, 4_000, "4 Mbps");
                            ui.selectable_value(&mut v.bitrate_kbps, 8_000, "8 Mbps");
                            ui.selectable_value(&mut v.bitrate_kbps, 12_000, "12 Mbps");
                            ui.selectable_value(&mut v.bitrate_kbps, 20_000, "20 Mbps");
                            ui.selectable_value(&mut v.bitrate_kbps, 35_000, "35 Mbps");
                        });
                    if v.bitrate_kbps != prev_br {
                        adjust_encoder(&self.state, &machine_id, v.bitrate_kbps, v.target_fps);
                    }

                    // FPS selector
                    let prev_fps = v.target_fps;
                    egui::ComboBox::from_id_salt("viewer_fps")
                        .width(72.0)
                        .selected_text(format!("{} FPS", v.target_fps))
                        .show_ui(ui, |ui| {
                            ui.selectable_value(&mut v.target_fps, 30, "30 FPS");
                            ui.selectable_value(&mut v.target_fps, 60, "60 FPS");
                            ui.selectable_value(&mut v.target_fps, 120, "120 FPS");
                        });
                    if v.target_fps != prev_fps {
                        adjust_encoder(&self.state, &machine_id, v.bitrate_kbps, v.target_fps);
                    }

                    // Input Enable/Disable Toggle (F9)
                    let inp_btn = egui::Button::new(
                        RichText::new(if v.input_enabled {
                            "Control: ACTIVO (F9)"
                        } else {
                            "Control: PAUSADO (F9)"
                        })
                        .size(11.0)
                        .strong()
                        .color(if v.input_enabled {
                            Color32::from_rgb(9, 11, 16)
                        } else {
                            AMBER
                        }),
                    )
                    .fill(if v.input_enabled {
                        EMERALD
                    } else {
                        Color32::from_rgba_unmultiplied(245, 158, 11, 28)
                    })
                    .stroke(Stroke::new(
                        1.0_f32,
                        if v.input_enabled { EMERALD } else { AMBER },
                    ))
                    .rounding(7.0);
                    if ui.add(inp_btn).clicked() {
                        v.input_enabled = !v.input_enabled;
                        if !v.input_enabled {
                            send_critical_input(
                                &self.state,
                                &machine_id,
                                CriticalInputEvent::ReleaseAll,
                            );
                        }
                    }

                    if ui.small_button("IDR").on_hover_text("Forzar fotograma clave").clicked() {
                        request_idr(&self.state, &machine_id);
                    }

                    if ui
                        .small_button(if v.show_hud { "HUD ✓" } else { "HUD" })
                        .clicked()
                    {
                        v.show_hud = !v.show_hud;
                    }

                    if ui
                        .small_button(if v.is_fullscreen { "Salir Full" } else { "Pantalla Completa" })
                        .clicked()
                    {
                        v.is_fullscreen = !v.is_fullscreen;
                        ctx.send_viewport_cmd(egui::ViewportCommand::Fullscreen(v.is_fullscreen));
                    }

                    ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                        if ui
                            .add(
                                egui::Button::new(
                                    RichText::new("Finalizar")
                                        .size(12.0)
                                        .strong()
                                        .color(Color32::WHITE),
                                )
                                .fill(RED)
                                .rounding(7.0)
                                .min_size(Vec2::new(82.0, 28.0)),
                            )
                            .clicked()
                        {
                            want_close = true;
                        }
                    });
                });
            });

        // Switch monitor if requested
        if let Some(new_disp) = switch_display {
            let v = self.viewer.as_mut().unwrap();
            v.display_index = new_disp;
            v.epoch = self.state.next_input_epoch();
            v.move_seq = 0;
            let _ = start_remote_session(
                &self.state,
                &machine_id,
                new_disp,
                v.target_fps,
                v.bitrate_kbps,
                VideoCodec::H264,
            );
        }

        // Bottom Telemetry HUD Bar
        if self.viewer.as_ref().is_some_and(|v| v.show_hud) {
            let v = self.viewer.as_ref().unwrap();
            egui::TopBottomPanel::bottom("viewer_hud")
                .exact_height(30.0)
                .frame(
                    Frame::none()
                        .fill(Color32::from_rgb(8, 10, 14))
                        .stroke(Stroke::new(1.0_f32, BORDER_SUBTLE))
                        .inner_margin(Margin::symmetric(14.0, 4.0)),
                )
                .show(ctx, |ui| {
                    ui.horizontal_centered(|ui| {
                        let fps_disp = if v.measured_fps > 0 {
                            v.measured_fps
                        } else {
                            agent.telemetry.fps.round() as u32
                        };
                        ui.label(
                            RichText::new(format!(
                                "FPS: {}   |   RTT: {} ms   |   CAP: {:.1} ms   |   ENC: {:.1} ms   |   DEC: {:.1} ms   |   RATE: {:.1} Mbps   |   {}x{}   |   QUIC TLS 1.3",
                                fps_disp,
                                agent.rtt_ms,
                                agent.telemetry.capture_ms,
                                agent.telemetry.encode_ms,
                                agent.telemetry.decode_ms,
                                agent.telemetry.bitrate_kbps as f32 / 1000.0,
                                agent.stream_width,
                                agent.stream_height,
                            ))
                            .monospace()
                            .size(11.0)
                            .color(TEXT_MUTED),
                        );

                        if agent.is_same_pc && !v.input_enabled {
                            ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                                ui.label(
                                    RichText::new(
                                        "Mismo PC detectado: control pausado para evitar bucle (pulsa F9 para activarlo)",
                                    )
                                    .size(11.0)
                                    .color(AMBER),
                                );
                            });
                        }
                    });
                });
        }

        // Central Native GPU Video Surface + Direct Mouse & Keyboard Capture
        egui::CentralPanel::default()
            .frame(Frame::none().fill(Color32::BLACK).inner_margin(Margin::same(4.0)))
            .show(ctx, |ui| {
                let avail = ui.available_rect_before_wrap();
                let v = self.viewer.as_mut().unwrap();

                if let Some(ref err) = v.error_banner {
                    ui.centered_and_justified(|ui| {
                        ui.label(RichText::new(err).size(14.0).color(RED));
                    });
                    return;
                }

                let Some(ref tex) = v.texture else {
                    ui.centered_and_justified(|ui| {
                        ui.vertical_centered(|ui| {
                            ui.spinner();
                            ui.add_space(10.0);
                            ui.label(
                                RichText::new("Esperando primer fotograma hardware (DXGI → GPU → QUIC)...")
                                    .size(13.5)
                                    .color(TEXT_PRIMARY),
                            );
                        });
                    });
                    return;
                };

                // Preserve exact aspect ratio of remote desktop inside available viewport
                let tex_size = tex.size_vec2();
                let aspect = (tex_size.x / tex_size.y.max(1.0)).max(0.1);
                let mut draw_w = avail.width();
                let mut draw_h = draw_w / aspect;
                if draw_h > avail.height() {
                    draw_h = avail.height();
                    draw_w = draw_h * aspect;
                }
                let video_rect = Rect::from_center_size(avail.center(), Vec2::new(draw_w, draw_h));

                // Allocate interactive area covering the exact video surface
                let response = ui.allocate_rect(video_rect, Sense::click_and_drag());

                // Paint the GPU-decoded remote desktop texture
                ui.painter().image(
                    tex.id(),
                    video_rect,
                    Rect::from_min_max(Pos2::ZERO, Pos2::new(1.0, 1.0)),
                    Color32::WHITE,
                );

                // Subtle emerald border when remote input is actively armed
                if v.input_enabled && response.hovered() {
                    ui.painter().rect_stroke(
                        video_rect,
                        Rounding::ZERO,
                        Stroke::new(1.5_f32, Color32::from_rgba_unmultiplied(16, 185, 129, 140))
                    );
                }

                // === DIRECT NATIVE MOUSE & KEYBOARD INJECTION ===
                if v.input_enabled {
                    let normalize = |pos: Pos2| -> (u16, u16) {
                        let nx = ((pos.x - video_rect.left()) / video_rect.width().max(1.0))
                            .clamp(0.0, 1.0);
                        let ny = ((pos.y - video_rect.top()) / video_rect.height().max(1.0))
                            .clamp(0.0, 1.0);
                        ((nx * 65535.0).round() as u16, (ny * 65535.0).round() as u16)
                    };

                    // 1. Mouse Movement (QUIC Unreliable Datagram — instant, zero HoL blocking!)
                    let pointer_pos = ctx.input(|i| i.pointer.hover_pos().or(i.pointer.latest_pos()));
                    if let Some(pos) = pointer_pos {
                        if video_rect.contains(pos) {
                            let (x, y) = normalize(pos);
                            let moved = (x, y) != v.last_sent_xy;
                            if moved && v.last_move_sent.elapsed() >= Duration::from_millis(6) {
                                v.move_seq = v.move_seq.wrapping_add(1);
                                v.last_sent_xy = (x, y);
                                v.last_move_sent = Instant::now();
                                send_mouse_move(
                                    &self.state,
                                    &machine_id,
                                    v.display_index as u8,
                                    v.epoch,
                                    x,
                                    y,
                                    v.move_seq,
                                );
                            }
                        }
                    }

                    // 2. Reliable Critical Events: Mouse Buttons, Wheel & Keyboard ScanCodes
                    let (events, cur_mods, focused) =
                        ctx.input(|i| (i.events.clone(), i.modifiers, i.focused));

                    if !focused {
                        if v.last_modifiers != egui::Modifiers::NONE {
                            v.last_modifiers = egui::Modifiers::NONE;
                            send_critical_input(
                                &self.state,
                                &machine_id,
                                CriticalInputEvent::ReleaseAll,
                            );
                        }
                    } else {
                        // Sync modifier keys (Shift, Ctrl, Alt, Win) cleanly
                        sync_modifier(
                            &self.state,
                            &machine_id,
                            v.last_modifiers.shift,
                            cur_mods.shift,
                            SCANCODE_LSHIFT,
                        );
                        sync_modifier(
                            &self.state,
                            &machine_id,
                            v.last_modifiers.ctrl,
                            cur_mods.ctrl,
                            SCANCODE_LCTRL,
                        );
                        sync_modifier(
                            &self.state,
                            &machine_id,
                            v.last_modifiers.alt,
                            cur_mods.alt,
                            SCANCODE_LALT,
                        );
                        sync_modifier(
                            &self.state,
                            &machine_id,
                            v.last_modifiers.mac_cmd || v.last_modifiers.command && !cur_mods.ctrl,
                            cur_mods.mac_cmd || cur_mods.command && !cur_mods.ctrl,
                            SCANCODE_LWIN,
                        );
                        v.last_modifiers = cur_mods;

                        for ev in events {
                            match ev {
                                egui::Event::PointerButton {
                                    pos,
                                    button,
                                    pressed,
                                    ..
                                } => {
                                    if video_rect.contains(pos) {
                                        let (x, y) = normalize(pos);
                                        let btn = match button {
                                            PointerButton::Primary => MouseButton::Left,
                                            PointerButton::Secondary => MouseButton::Right,
                                            PointerButton::Middle => MouseButton::Middle,
                                            PointerButton::Extra1 => MouseButton::X1,
                                            PointerButton::Extra2 => MouseButton::X2,
                                        };
                                        send_critical_input(
                                            &self.state,
                                            &machine_id,
                                            CriticalInputEvent::MouseButton {
                                                button: btn,
                                                pressed,
                                                x,
                                                y,
                                            },
                                        );
                                    }
                                }
                                egui::Event::MouseWheel { delta, .. } => {
                                    if response.hovered() {
                                        let dy = (delta.y * 40.0).clamp(-360.0, 360.0) as i16;
                                        let dx = (delta.x * 40.0).clamp(-360.0, 360.0) as i16;
                                        if dx != 0 || dy != 0 {
                                            send_critical_input(
                                                &self.state,
                                                &machine_id,
                                                CriticalInputEvent::MouseWheel {
                                                    delta_x: dx,
                                                    delta_y: dy,
                                                },
                                            );
                                        }
                                    }
                                }
                                egui::Event::Key {
                                    key,
                                    physical_key,
                                    pressed,
                                    ..
                                } => {
                                    // Don't forward F9 (our local input toggle hotkey)
                                    if key == egui::Key::F9 {
                                        continue;
                                    }
                                    let target_key = physical_key.unwrap_or(key);
                                    if let Some((scancode, extended)) =
                                        egui_key_to_scancode(target_key)
                                    {
                                        send_critical_input(
                                            &self.state,
                                            &machine_id,
                                            CriticalInputEvent::Key {
                                                scancode,
                                                extended,
                                                pressed,
                                            },
                                        );
                                    }
                                }
                                _ => {}
                            }
                        }
                    }
                }
            });

        if want_close {
            self.close_viewer(ctx);
        }
    }
}

fn sync_modifier(
    state: &AdminState,
    machine_id: &str,
    was_down: bool,
    is_down: bool,
    (scancode, extended): (u16, bool),
) {
    if was_down != is_down {
        send_critical_input(
            state,
            machine_id,
            CriticalInputEvent::Key {
                scancode,
                extended,
                pressed: is_down,
            },
        );
    }
}

fn info_row(ui: &mut egui::Ui, label: &str, value: &str, value_color: Color32) {
    ui.horizontal(|ui| {
        ui.label(RichText::new(label).monospace().size(11.0).color(TEXT_MUTED));
        ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
            ui.label(
                RichText::new(value)
                    .monospace()
                    .size(11.0)
                    .color(value_color),
            );
        });
    });
}

fn apply_gecko_theme(ctx: &egui::Context) {
    let mut visuals = egui::Visuals::dark();
    visuals.window_fill = BG_CARD;
    visuals.panel_fill = BG_APP;
    visuals.extreme_bg_color = Color32::from_rgb(7, 9, 13);
    visuals.widgets.noninteractive.bg_fill = BG_CARD;
    visuals.widgets.noninteractive.bg_stroke = Stroke::new(1.0_f32, BORDER_SUBTLE);
    visuals.widgets.inactive.bg_fill = BG_CARD_HOVER;
    visuals.widgets.inactive.bg_stroke = Stroke::new(1.0_f32, BORDER_SUBTLE);
    visuals.widgets.hovered.bg_fill = Color32::from_rgb(28, 34, 52);
    visuals.widgets.hovered.bg_stroke = Stroke::new(1.0_f32, EMERALD);
    visuals.widgets.active.bg_fill = Color32::from_rgb(32, 42, 62);
    visuals.selection.bg_fill = Color32::from_rgba_unmultiplied(16, 185, 129, 70);
    visuals.selection.stroke = Stroke::new(1.0_f32, EMERALD);
    ctx.set_visuals(visuals);
}
