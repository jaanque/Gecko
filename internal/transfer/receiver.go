package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"remote-control/internal/protocol"
)

type activeTransfer struct {
	id         string
	name       string
	expectedSz int64
	expectedSH string
	received   int64
	partPath   string
	destDir    string
	file       *os.File
	hasher     hash.Hash
}

// Receiver manages safe incoming file transfers into a target directory with
// filename sanitization, size limits, unique naming (no overwrites), and SHA-256 verification.
type Receiver struct {
	destDir string

	mu     sync.Mutex
	active map[string]*activeTransfer
}

// NewReceiver constructs a Receiver that writes completed files into destDir.
// If destDir is empty, DefaultDownloadDir() is used.
func NewReceiver(destDir string) *Receiver {
	if strings.TrimSpace(destDir) == "" {
		destDir = DefaultDownloadDir()
	}
	return &Receiver{
		destDir: destDir,
		active:  make(map[string]*activeTransfer),
	}
}

// DefaultDownloadDir returns ~/Downloads if present/creatable, falling back to home or temp dir.
func DefaultDownloadDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dl := filepath.Join(home, "Downloads")
		if info, err := os.Stat(dl); err == nil && info.IsDir() {
			return dl
		}
		if err := os.MkdirAll(dl, 0o755); err == nil {
			return dl
		}
		return home
	}
	return os.TempDir()
}

// StartOffer validates an incoming FileOfferPayload and prepares a temporary .part file.
func (r *Receiver) StartOffer(offer protocol.FileOfferPayload) error {
	if offer.TransferID == "" || len(offer.TransferID) > 64 {
		return errors.New("transfer_id inválido")
	}
	if offer.Size < 0 || offer.Size > protocol.MaxFileTransferSize {
		return fmt.Errorf("tamaño de archivo no permitido (%d bytes)", offer.Size)
	}
	if len(offer.SHA256) != 64 {
		return errors.New("checksum SHA-256 inválido")
	}
	if _, err := hex.DecodeString(offer.SHA256); err != nil {
		return errors.New("checksum SHA-256 malformado")
	}
	if err := os.MkdirAll(r.destDir, 0o755); err != nil {
		return fmt.Errorf("no se pudo acceder a la carpeta de descargas: %w", err)
	}

	safeName := protocol.SanitizeFileName(offer.Name)

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.active) >= 4 {
		return errors.New("demasiadas transferencias simultáneas")
	}
	if _, exists := r.active[offer.TransferID]; exists {
		return errors.New("transferencia duplicada")
	}

	f, err := os.CreateTemp(r.destDir, ".gecko-recv-*.part")
	if err != nil {
		return err
	}

	r.active[offer.TransferID] = &activeTransfer{
		id:         offer.TransferID,
		name:       safeName,
		expectedSz: offer.Size,
		expectedSH: strings.ToLower(offer.SHA256),
		partPath:   f.Name(),
		destDir:    r.destDir,
		file:       f,
		hasher:     sha256.New(),
	}
	return nil
}

// WriteChunk appends one sequential chunk to an active transfer.
func (r *Receiver) WriteChunk(ch protocol.FileChunk) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	at, ok := r.active[ch.TransferID]
	if !ok {
		return errors.New("transferencia desconocida")
	}
	if int64(ch.Offset) != at.received {
		return fmt.Errorf("offset fuera de orden: got %d want %d", ch.Offset, at.received)
	}
	if at.received+int64(len(ch.Data)) > at.expectedSz {
		return errors.New("el archivo excede el tamaño declarado")
	}
	if len(ch.Data) == 0 {
		return nil
	}
	if _, err := at.file.Write(ch.Data); err != nil {
		return err
	}
	_, _ = at.hasher.Write(ch.Data)
	at.received += int64(len(ch.Data))
	return nil
}

// Finish verifies exact byte count and SHA-256 checksum, then atomically moves the file
// into a non-colliding path inside destDir and returns the final saved path.
func (r *Receiver) Finish(transferID string) (string, error) {
	r.mu.Lock()
	at, ok := r.active[transferID]
	if ok {
		delete(r.active, transferID)
	}
	r.mu.Unlock()

	if !ok {
		return "", errors.New("transferencia desconocida")
	}

	cleanup := func() {
		_ = at.file.Close()
		_ = os.Remove(at.partPath)
	}

	if at.received != at.expectedSz {
		cleanup()
		return "", fmt.Errorf("archivo incompleto (%d/%d bytes)", at.received, at.expectedSz)
	}
	gotHash := hex.EncodeToString(at.hasher.Sum(nil))
	if gotHash != at.expectedSH {
		cleanup()
		return "", errors.New("error de integridad SHA-256")
	}
	if err := at.file.Close(); err != nil {
		_ = os.Remove(at.partPath)
		return "", err
	}

	finalPath := uniqueDestPath(at.destDir, at.name)
	if err := os.Rename(at.partPath, finalPath); err != nil {
		_ = os.Remove(at.partPath)
		return "", err
	}
	return finalPath, nil
}

// AbortAll cancels and removes all in-progress temporary files (e.g. on session end).
func (r *Receiver) AbortAll() {
	r.mu.Lock()
	pending := r.active
	r.active = make(map[string]*activeTransfer)
	r.mu.Unlock()

	for _, at := range pending {
		_ = at.file.Close()
		_ = os.Remove(at.partPath)
	}
}

func uniqueDestPath(dir, name string) string {
	candidate := filepath.Join(dir, name)
	if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
		return candidate
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i < 1000; i++ {
		c := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		if _, err := os.Stat(c); errors.Is(err, os.ErrNotExist) {
			return c
		}
	}
	return candidate
}
