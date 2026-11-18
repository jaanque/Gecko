use std::sync::Arc;
use std::time::Duration;

use anyhow::Result;
use quinn::crypto::rustls::{QuicClientConfig, QuicServerConfig};
use quinn::{ClientConfig, ServerConfig, TransportConfig, VarInt};
use rmm_proto::ALPN_RMM_V1;
use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer, ServerName, UnixTime};
use rustls::{DigitallySignedStruct, Error as TlsError, SignatureScheme};

use crate::identity::{sha256_bytes, AdminIdentity};

fn install_ring_provider() {
    let _ = rustls::crypto::ring::default_provider().install_default();
}

/// Shared low-latency QUIC transport tuning:
/// - Unreliable Datagrams enabled (RFC 9221) for zero-HoL video & mouse move
/// - Short keepalive for NAT hole maintenance
/// - Bounded receive/send datagram buffers so old video frames never queue up
fn build_transport_config() -> Arc<TransportConfig> {
    let mut transport = TransportConfig::default();
    transport.keep_alive_interval(Some(Duration::from_secs(2)));
    transport.max_idle_timeout(Some(VarInt::from_u32(10_000).into()));
    // Enable QUIC Unreliable Datagrams (RFC 9221)
    transport.datagram_receive_buffer_size(Some(2 * 1024 * 1024));
    transport.datagram_send_buffer_size(2 * 1024 * 1024);
    transport.max_concurrent_bidi_streams(VarInt::from_u32(16));
    transport.max_concurrent_uni_streams(VarInt::from_u32(16));
    Arc::new(transport)
}

/// Build the Admin QUIC ServerConfig using the persisted TLS certificate.
pub fn build_server_config(identity: &AdminIdentity) -> Result<ServerConfig> {
    install_ring_provider();

    let cert = CertificateDer::from(identity.cert_der.clone());
    let key = PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(identity.key_der.clone()));

    let mut tls_config = rustls::ServerConfig::builder()
        .with_no_client_auth()
        .with_single_cert(vec![cert], key)?;
    tls_config.alpn_protocols = vec![ALPN_RMM_V1.to_vec()];
    tls_config.max_early_data_size = 0;

    let quic_crypto = QuicServerConfig::try_from(tls_config)?;
    let mut server_config = ServerConfig::with_crypto(Arc::new(quic_crypto));
    server_config.transport_config(build_transport_config());

    Ok(server_config)
}

/// Build the Agent QUIC ClientConfig that strictly pins the Admin's TLS certificate SHA-256.
pub fn build_pinned_client_config(expected_cert_sha256: [u8; 32]) -> Result<ClientConfig> {
    install_ring_provider();

    let verifier = Arc::new(PinnedCertVerifier {
        expected_sha256: expected_cert_sha256,
        provider: rustls::crypto::ring::default_provider(),
    });

    let mut tls_config = rustls::ClientConfig::builder()
        .dangerous()
        .with_custom_certificate_verifier(verifier)
        .with_no_client_auth();
    tls_config.alpn_protocols = vec![ALPN_RMM_V1.to_vec()];

    let quic_crypto = QuicClientConfig::try_from(tls_config)?;
    let mut client_config = ClientConfig::new(Arc::new(quic_crypto));
    client_config.transport_config(build_transport_config());

    Ok(client_config)
}

/// Custom TLS certificate verifier performing strict certificate pinning against `expected_sha256`.
#[derive(Debug)]
struct PinnedCertVerifier {
    expected_sha256: [u8; 32],
    provider: rustls::crypto::CryptoProvider,
}

impl ServerCertVerifier for PinnedCertVerifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        _intermediates: &[CertificateDer<'_>],
        _server_name: &ServerName<'_>,
        _ocsp_response: &[u8],
        _now: UnixTime,
    ) -> Result<ServerCertVerified, TlsError> {
        let actual = sha256_bytes(end_entity.as_ref());
        if actual == self.expected_sha256 {
            Ok(ServerCertVerified::assertion())
        } else {
            Err(TlsError::InvalidCertificate(
                rustls::CertificateError::BadSignature,
            ))
        }
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, TlsError> {
        rustls::crypto::verify_tls12_signature(
            message,
            cert,
            dss,
            &self.provider.signature_verification_algorithms,
        )
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, TlsError> {
        rustls::crypto::verify_tls13_signature(
            message,
            cert,
            dss,
            &self.provider.signature_verification_algorithms,
        )
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.provider
            .signature_verification_algorithms
            .supported_schemes()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::framing::{read_framed, write_framed};
    use crate::reassembler::{fragment_video_frame, FrameReassembler};
    use quinn::Endpoint;
    use rmm_proto::{AdminControlMsg, VIDEO_FLAG_KEYFRAME};
    use std::net::SocketAddr;

    #[tokio::test]
    async fn quic_pinned_tls_and_video_datagram_end_to_end() {
        let identity = AdminIdentity::generate().unwrap();
        let server_cfg = build_server_config(&identity).unwrap();
        let server = Endpoint::server(server_cfg, "127.0.0.1:0".parse::<SocketAddr>().unwrap()).unwrap();
        let server_addr = server.local_addr().unwrap();

        let client_cfg = build_pinned_client_config(identity.cert_sha256).unwrap();
        let mut client = Endpoint::client("127.0.0.1:0".parse::<SocketAddr>().unwrap()).unwrap();
        client.set_default_client_config(client_cfg);

        let srv_task = tokio::spawn(async move {
            let incoming = server.accept().await.unwrap();
            let conn = incoming.await.unwrap();
            let (mut send, mut recv) = conn.accept_bi().await.unwrap();
            let msg: AdminControlMsg = read_framed(&mut recv).await.unwrap();
            assert!(matches!(msg, AdminControlMsg::RequestIdr));
            write_framed(&mut send, &AdminControlMsg::Heartbeat { nonce: 42 }).await.unwrap();

            // Receive fragmented video datagrams
            let mut reassembler = FrameReassembler::new();
            loop {
                let dgram = conn.read_datagram().await.unwrap();
                let res = reassembler.push_datagram(&dgram);
                if let Some(frame) = res.frame {
                    return frame;
                }
            }
        });

        let conn = client.connect(server_addr, "gecko-admin.local").unwrap().await.unwrap();
        let (mut send, mut recv) = conn.open_bi().await.unwrap();
        write_framed(&mut send, &AdminControlMsg::RequestIdr).await.unwrap();
        let reply: AdminControlMsg = read_framed(&mut recv).await.unwrap();
        assert!(matches!(reply, AdminControlMsg::Heartbeat { nonce: 42 }));

        // Blast a 6 KB simulated H.264 IDR frame over QUIC unreliable datagrams
        let payload = vec![0xA5u8; 6_000];
        let frags = fragment_video_frame(1, VIDEO_FLAG_KEYFRAME, 999, &payload, 1180);
        for frag in frags {
            conn.send_datagram(frag).unwrap();
        }

        let assembled = srv_task.await.unwrap();
        assert!(assembled.is_keyframe);
        assert_eq!(assembled.data, payload);
    }

    #[tokio::test]
    async fn quic_rejects_mismatched_pinned_certificate() {
        let identity = AdminIdentity::generate().unwrap();
        let server_cfg = build_server_config(&identity).unwrap();
        let server = Endpoint::server(server_cfg, "127.0.0.1:0".parse::<SocketAddr>().unwrap()).unwrap();
        let server_addr = server.local_addr().unwrap();

        // Wrong SHA-256 pin
        let wrong_pin = [0xFFu8; 32];
        let client_cfg = build_pinned_client_config(wrong_pin).unwrap();
        let mut client = Endpoint::client("127.0.0.1:0".parse::<SocketAddr>().unwrap()).unwrap();
        client.set_default_client_config(client_cfg);

        let srv_handle = tokio::spawn(async move {
            if let Some(incoming) = server.accept().await {
                let _ = incoming.await;
            }
        });

        let res = client.connect(server_addr, "gecko-admin.local").unwrap().await;
        assert!(res.is_err(), "Client must reject server with unpinned certificate");
        srv_handle.abort();
    }
}
