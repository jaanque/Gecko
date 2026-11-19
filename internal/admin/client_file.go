package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"remote-control/internal/protocol"
	"remote-control/internal/security"
)

// SendFile transfers a local file to the active remote client with SHA-256 integrity verification.
func (c *RelayClient) SendFile(filePath string) error {
	conn := c.currentConn()
	if conn == nil {
		return errors.New("no hay conexión con el servidor puente")
	}
	c.mu.RLock()
	inSess := c.activeSession != ""
	c.mu.RUnlock()
	if !inSess {
		return errors.New("no hay ninguna sesión remota activa")
	}

	info, err := os.Stat(filePath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return errors.New("selecciona un archivo, no una carpeta")
	}
	if info.Size() > protocol.MaxFileTransferSize {
		return fmt.Errorf("el archivo supera el límite de %d MB", protocol.MaxFileTransferSize/(1024*1024))
	}

	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return err
	}
	sumHex := hex.EncodeToString(hasher.Sum(nil))
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	txID, err := security.RandomHex(8)
	if err != nil {
		return err
	}
	safeName := protocol.SanitizeFileName(filepath.Base(filePath))

	acceptCh := make(chan protocol.FileAcceptPayload, 1)
	doneCh := make(chan protocol.FileDonePayload, 1)
	c.fileMu.Lock()
	c.fileAcceptCh[txID] = acceptCh
	c.fileDoneCh[txID] = doneCh
	c.fileMu.Unlock()
	defer func() {
		c.fileMu.Lock()
		delete(c.fileAcceptCh, txID)
		delete(c.fileDoneCh, txID)
		c.fileMu.Unlock()
	}()

	c.emitFileStatus(safeName, true, "Enviando "+safeName+"...")

	if err := c.writeJSON(conn, protocol.MsgFileOffer, protocol.FileOfferPayload{
		TransferID: txID,
		Name:       safeName,
		Size:       info.Size(),
		SHA256:     sumHex,
	}); err != nil {
		c.emitFileStatus(safeName, false, "Error enviando oferta")
		return err
	}

	select {
	case acc := <-acceptCh:
		if !acc.Accepted {
			reason := acc.Reason
			if reason == "" {
				reason = "transferencia rechazada"
			}
			c.emitFileStatus(safeName, false, reason)
			return errors.New(reason)
		}
	case <-time.After(15 * time.Second):
		c.emitFileStatus(safeName, false, "sin respuesta del equipo remoto")
		return errors.New("sin respuesta del equipo remoto al enviar archivo")
	}

	buf := make([]byte, protocol.FileChunkSize)
	var offset uint64
	lastPct := -1
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			pkt, err := protocol.EncodeFileChunk(protocol.FileChunk{
				TransferID: txID,
				Offset:     offset,
				Data:       buf[:n],
			})
			if err != nil {
				return err
			}
			if err := c.writeMessage(conn, protocol.MsgFileChunk, pkt); err != nil {
				c.emitFileStatus(safeName, false, "error durante el envío")
				return err
			}
			offset += uint64(n)
			if info.Size() > 0 {
				pct := int((int64(offset) * 100) / info.Size())
				if pct >= lastPct+10 || pct == 100 {
					lastPct = pct
					c.emitFileStatus(safeName, true, fmt.Sprintf("Enviando %s (%d%%)...", safeName, pct))
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}

	if err := c.writeJSON(conn, protocol.MsgFileDone, protocol.FileDonePayload{
		TransferID: txID,
		OK:         true,
	}); err != nil {
		c.emitFileStatus(safeName, false, "error cerrando envío")
		return err
	}

	select {
	case done := <-doneCh:
		if !done.OK {
			msg := done.Error
			if msg == "" {
				msg = "error al guardar el archivo en el equipo remoto"
			}
			c.emitFileStatus(safeName, false, msg)
			return errors.New(msg)
		}
		detail := "Archivo guardado en el equipo remoto"
		if done.SavedPath != "" {
			detail = "Guardado en " + done.SavedPath
		}
		c.emitFileStatus(safeName, true, detail)
		return nil
	case <-time.After(20 * time.Second):
		c.emitFileStatus(safeName, false, "sin confirmación final del equipo remoto")
		return errors.New("sin confirmación final del equipo remoto")
	}
}
