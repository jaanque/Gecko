#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

mod generator;
mod scancodes;
mod server;
mod state;
mod ui;

use std::sync::Arc;

use directories::ProjectDirs;
use eframe::egui;
use rmm_input_win::enable_per_monitor_dpi_awareness;
use rmm_proto::{DEFAULT_DISCOVERY_PORT, DEFAULT_QUIC_PORT};
use tracing::{error, info};

use crate::state::AdminState;
use crate::ui::GeckoAdminApp;

fn main() -> eframe::Result<()> {
    enable_per_monitor_dpi_awareness();

    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "gecko_admin=info,rmm_net=info,rmm_decode_win=info".into()),
        )
        .init();

    let data_dir = ProjectDirs::from("com", "gecko", "admin")
        .map(|d| d.data_local_dir().to_path_buf())
        .unwrap_or_else(|| std::env::temp_dir().join("gecko-admin"));

    let quic_port = std::env::var("GECKO_PORT")
        .ok()
        .and_then(|s| s.parse::<u16>().ok())
        .unwrap_or(DEFAULT_QUIC_PORT);

    let discovery_port = std::env::var("GECKO_DISCOVERY_PORT")
        .ok()
        .and_then(|s| s.parse::<u16>().ok())
        .unwrap_or(DEFAULT_DISCOVERY_PORT);

    let admin_state = Arc::new(
        AdminState::initialize(data_dir.clone(), quic_port, discovery_port)
            .expect("Failed to initialize Gecko Admin cryptographic identity"),
    );

    info!(
        data_dir = %data_dir.display(),
        quic_port,
        discovery_port,
        cert_fp = %admin_state.identity.cert_fingerprint_hex(),
        "Gecko Native Desktop Admin starting"
    );

    // Spawn embedded Tokio runtime for QUIC Reverse-Connection Server + LAN Auto-Discovery
    let server_state = admin_state.clone();
    std::thread::Builder::new()
        .name("gecko-quic-runtime".into())
        .spawn(move || {
            let rt = tokio::runtime::Builder::new_multi_thread()
                .enable_all()
                .build()
                .expect("Failed to build Tokio runtime");
            rt.block_on(async move {
                if let Err(e) = server::run_admin_servers(server_state).await {
                    error!("Embedded QUIC server terminated: {e:?}");
                }
            });
        })
        .expect("Failed to spawn QUIC server thread");

    let native_options = eframe::NativeOptions {
        viewport: egui::ViewportBuilder::default()
            .with_title("Gecko — Control Remoto Nativo")
            .with_inner_size([1320.0, 840.0])
            .with_min_inner_size([960.0, 620.0]),
        ..Default::default()
    };

    eframe::run_native(
        "Gecko",
        native_options,
        Box::new(move |cc| Ok(Box::new(GeckoAdminApp::new(cc, admin_state)))),
    )
}
