package config

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
)

const (
	trailerMagic   = "RMTCONF1"
	trailerFooter  = 16 // 4 bytes length + 4 bytes crc32 + 8 bytes magic
	maxConfigBytes = 512 * 1024
)

var ErrNoEmbeddedConfig = errors.New("no embedded client config found")

// ClientConfig holds the connection, security, and optional branding parameters embedded into ClienteRemoto.exe.
type ClientConfig struct {
	ServerAddr string `json:"server_addr"`
	CertSHA256 string `json:"cert_sha256"`
	AuthToken  string `json:"auth_token"`
	LogoPNG    []byte `json:"logo_png,omitempty"`
}

// Validate checks that all required fields are present.
func (c ClientConfig) Validate() error {
	if c.ServerAddr == "" {
		return errors.New("server_addr is required")
	}
	if len(c.CertSHA256) != 64 {
		return errors.New("cert_sha256 must be a 64-char hex SHA-256 fingerprint")
	}
	if len(c.AuthToken) < 16 {
		return errors.New("auth_token is invalid")
	}
	return nil
}

// AppendToBinary strips any prior config trailer from baseExe and appends cfg as a verified trailer.
func AppendToBinary(baseExe []byte, cfg ClientConfig) ([]byte, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	base := stripExistingTrailer(baseExe)

	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxConfigBytes {
		return nil, fmt.Errorf("config too large: %d bytes", len(raw))
	}

	out := make([]byte, 0, len(base)+len(raw)+trailerFooter)
	out = append(out, base...)
	out = append(out, raw...)

	var footer [trailerFooter]byte
	binary.BigEndian.PutUint32(footer[0:4], uint32(len(raw)))
	binary.BigEndian.PutUint32(footer[4:8], crc32.ChecksumIEEE(raw))
	copy(footer[8:16], trailerMagic)
	out = append(out, footer[:]...)
	return out, nil
}

// ReadFromExecutable reads the embedded ClientConfig from the currently running binary.
func ReadFromExecutable() (ClientConfig, error) {
	exePath, err := os.Executable()
	if err != nil {
		return ClientConfig{}, err
	}
	f, err := os.Open(exePath)
	if err != nil {
		return ClientConfig{}, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return ClientConfig{}, err
	}
	return ReadFromReaderAt(f, info.Size())
}

// ReadFromReaderAt parses the embedded config trailer from r with total length size.
func ReadFromReaderAt(r io.ReaderAt, size int64) (ClientConfig, error) {
	if size < trailerFooter {
		return ClientConfig{}, ErrNoEmbeddedConfig
	}
	var footer [trailerFooter]byte
	if _, err := r.ReadAt(footer[:], size-trailerFooter); err != nil {
		return ClientConfig{}, err
	}
	if string(footer[8:16]) != trailerMagic {
		return ClientConfig{}, ErrNoEmbeddedConfig
	}

	cfgLen := int64(binary.BigEndian.Uint32(footer[0:4]))
	expectedCRC := binary.BigEndian.Uint32(footer[4:8])
	if cfgLen <= 0 || cfgLen > maxConfigBytes || cfgLen+trailerFooter > size {
		return ClientConfig{}, errors.New("corrupt embedded config length")
	}

	raw := make([]byte, cfgLen)
	if _, err := r.ReadAt(raw, size-trailerFooter-cfgLen); err != nil {
		return ClientConfig{}, err
	}
	if crc32.ChecksumIEEE(raw) != expectedCRC {
		return ClientConfig{}, errors.New("embedded config checksum mismatch")
	}

	var cfg ClientConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return ClientConfig{}, fmt.Errorf("invalid embedded config JSON: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return ClientConfig{}, err
	}
	return cfg, nil
}

func stripExistingTrailer(data []byte) []byte {
	if len(data) < trailerFooter {
		return data
	}
	footer := data[len(data)-trailerFooter:]
	if string(footer[8:16]) != trailerMagic {
		return data
	}
	cfgLen := int(binary.BigEndian.Uint32(footer[0:4]))
	if cfgLen <= 0 || cfgLen > maxConfigBytes || cfgLen+trailerFooter > len(data) {
		return data
	}
	raw := data[len(data)-trailerFooter-cfgLen : len(data)-trailerFooter]
	expectedCRC := binary.BigEndian.Uint32(footer[4:8])
	if crc32.ChecksumIEEE(raw) != expectedCRC {
		return data
	}
	return data[:len(data)-trailerFooter-cfgLen]
}

// ReadFromBytes is a convenience helper for tests.
func ReadFromBytes(data []byte) (ClientConfig, error) {
	return ReadFromReaderAt(bytes.NewReader(data), int64(len(data)))
}
