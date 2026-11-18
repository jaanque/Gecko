use std::net::{IpAddr, Ipv4Addr, SocketAddr};
use std::time::Duration;

use anyhow::Result;
use rmm_proto::{LanDiscoveryProbe, LanDiscoveryReply};
use tokio::net::UdpSocket;
use tracing::{debug, info, warn};

/// Background responder run inside the Gecko Admin app.
/// Replies ONLY to LAN probes matching this Admin's exact TLS certificate SHA-256 prefix.
pub async fn run_lan_discovery_responder(
    admin_cert_sha256: [u8; 32],
    quic_port: u16,
    discovery_port: u16,
) -> Result<()> {
    let bind_addr = SocketAddr::new(IpAddr::V4(Ipv4Addr::UNSPECIFIED), discovery_port);
    let socket = UdpSocket::bind(bind_addr).await?;
    let expected = LanDiscoveryProbe::from_cert_sha256(&admin_cert_sha256).cert_prefix;
    let reply_bytes = LanDiscoveryReply {
        cert_prefix: expected,
        quic_port,
    }
    .encode();

    info!(%bind_addr, quic_port, "LAN Auto-Discovery responder active");

    let mut buf = [0u8; 128];
    loop {
        match socket.recv_from(&mut buf).await {
            Ok((len, peer)) => {
                if let Some(probe) = LanDiscoveryProbe::decode(&buf[..len]) {
                    if probe.cert_prefix == expected {
                        debug!(%peer, "Matched signed LAN discovery probe — replying");
                        let _ = socket.send_to(&reply_bytes, peer).await;
                    }
                }
            }
            Err(e) => {
                warn!("LAN discovery recv error: {e}");
                tokio::time::sleep(Duration::from_millis(200)).await;
            }
        }
    }
}

/// Broadcast a signed LAN discovery probe from the Agent to auto-locate the Gecko Admin
/// on the local network (handles DHCP IP changes with zero reconfiguration).
pub async fn discover_admin_on_lan(
    admin_cert_sha256: [u8; 32],
    discovery_port: u16,
    timeout: Duration,
) -> Option<SocketAddr> {
    let socket = UdpSocket::bind("0.0.0.0:0").await.ok()?;
    let _ = socket.set_broadcast(true);

    let probe = LanDiscoveryProbe::from_cert_sha256(&admin_cert_sha256);
    let raw_probe = probe.encode();

    let targets = [
        SocketAddr::new(IpAddr::V4(Ipv4Addr::BROADCAST), discovery_port),
        SocketAddr::new(IpAddr::V4(Ipv4Addr::LOCALHOST), discovery_port),
    ];

    for t in targets {
        let _ = socket.send_to(&raw_probe, t).await;
    }

    let mut buf = [0u8; 128];
    let recv_res = tokio::time::timeout(timeout, socket.recv_from(&mut buf)).await;
    if let Ok(Ok((len, peer))) = recv_res {
        if let Some(reply) = LanDiscoveryReply::decode(&buf[..len]) {
            if reply.cert_prefix == probe.cert_prefix && reply.quic_port > 0 {
                return Some(SocketAddr::new(peer.ip(), reply.quic_port));
            }
        }
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn lan_discovery_finds_matching_admin_and_ignores_wrong_cert() {
        let cert = [0x42u8; 32];
        let disc_port = 44_349;
        let quic_port = 4433;

        let task = tokio::spawn(async move {
            let _ = run_lan_discovery_responder(cert, quic_port, disc_port).await;
        });
        tokio::time::sleep(Duration::from_millis(60)).await;

        let found = discover_admin_on_lan(cert, disc_port, Duration::from_millis(400)).await;
        assert!(found.is_some(), "Should discover matching Admin on loopback/LAN");
        assert_eq!(found.unwrap().port(), quic_port);

        let wrong_cert = [0x99u8; 32];
        let not_found =
            discover_admin_on_lan(wrong_cert, disc_port, Duration::from_millis(150)).await;
        assert!(not_found.is_none(), "Must ignore probes with mismatched cert prefix");

        task.abort();
    }
}
