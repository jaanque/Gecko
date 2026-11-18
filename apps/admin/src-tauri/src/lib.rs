mod commands;
mod generator;
mod server;
mod state;

use std::sync::Arc;

use directories::ProjectDirs;
use state::AdminState;
use tracing::{error, info};

const DEFAULT_QUIC_PORT: u16 = 4433;

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "rmm_admin=info,rmm_net=info".into()),
        )
        .init();

    let data_dir = ProjectDirs::from("com", "gecko", "admin")
        .map(|d| d.data_local_dir().to_path_buf())
        .unwrap_or_else(|| std::env::temp_dir().join("gecko-admin"));

    let port = std::env::var("GECKO_PORT")
        .ok()
        .and_then(|s| s.parse::<u16>().ok())
        .unwrap_or(DEFAULT_QUIC_PORT);

    let admin_state = Arc::new(
        AdminState::initialize(data_dir.clone(), port)
            .expect("Failed to initialize Gecko Admin cryptographic identity"),
    );

    info!(
        data_dir = %data_dir.display(),
        port,
        cert_fp = %admin_state.identity.cert_fingerprint_hex(),
        "Gecko Admin initialized"
    );

    let server_state = admin_state.clone();

    tauri::Builder::default()
        .manage(admin_state)
        .setup(move |app| {
            let handle = app.handle().clone();
            tauri::async_runtime::spawn(async move {
                if let Err(e) = server::run_quic_server(server_state, handle).await {
                    error!("Embedded QUIC server terminated with error: {e:?}");
                }
            });
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            commands::get_server_info,
            commands::list_agents,
            commands::generate_client,
            commands::revoke_token,
            commands::reveal_in_explorer,
            commands::start_remote_session,
            commands::stop_remote_session,
            commands::send_mouse_move,
            commands::send_critical_input,
            commands::request_idr,
            commands::adjust_encoder,
            commands::disconnect_agent,
        ])
        .run(tauri::generate_context!())
        .expect("error while running Gecko Admin application");
}
