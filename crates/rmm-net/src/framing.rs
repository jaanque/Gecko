use anyhow::{bail, Result};
use quinn::{RecvStream, SendStream};
use serde::{de::DeserializeOwned, Serialize};

const MAX_CONTROL_FRAME_LEN: usize = 4 * 1024 * 1024; // 4 MiB safety limit

/// Write a length-prefixed `postcard` message onto a reliable QUIC stream.
pub async fn write_framed<T: Serialize>(stream: &mut SendStream, msg: &T) -> Result<()> {
    let payload = postcard::to_allocvec(msg)?;
    if payload.len() > MAX_CONTROL_FRAME_LEN {
        bail!("Control message exceeds max frame size: {}", payload.len());
    }
    let len_bytes = (payload.len() as u32).to_le_bytes();
    stream.write_all(&len_bytes).await?;
    stream.write_all(&payload).await?;
    Ok(())
}

/// Read a length-prefixed `postcard` message from a reliable QUIC stream.
pub async fn read_framed<T: DeserializeOwned>(stream: &mut RecvStream) -> Result<T> {
    let mut len_buf = [0u8; 4];
    stream.read_exact(&mut len_buf).await?;
    let len = u32::from_le_bytes(len_buf) as usize;
    if len == 0 || len > MAX_CONTROL_FRAME_LEN {
        bail!("Invalid control frame length: {}", len);
    }
    let mut payload = vec![0u8; len];
    stream.read_exact(&mut payload).await?;
    let msg = postcard::from_bytes(&payload)?;
    Ok(msg)
}
