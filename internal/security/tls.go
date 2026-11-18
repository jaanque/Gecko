package security

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ServerIdentity holds the persisted TLS certificate, its SHA-256 fingerprint, and the shared auth token.
type ServerIdentity struct {
	Certificate tls.Certificate
	CertSHA256  string
	AuthToken   string
}

type secretFile struct {
	AuthToken string `json:"auth_token"`
}

// RandomHex returns n random bytes encoded as lowercase hex.
func RandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// LoadOrCreateIdentity loads or creates server.crt, server.key, and secret.json inside dataDir.
func LoadOrCreateIdentity(dataDir string) (*ServerIdentity, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}

	certPath := filepath.Join(dataDir, "server.crt")
	keyPath := filepath.Join(dataDir, "server.key")
	secretPath := filepath.Join(dataDir, "secret.json")

	if !fileExists(certPath) || !fileExists(keyPath) {
		if err := generateSelfSignedCert(certPath, keyPath); err != nil {
			return nil, fmt.Errorf("generating TLS certificate: %w", err)
		}
	}

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("loading TLS key pair: %w", err)
	}
	if len(cert.Certificate) == 0 {
		return nil, errors.New("empty certificate chain")
	}
	sum := sha256.Sum256(cert.Certificate[0])
	fingerprint := hex.EncodeToString(sum[:])

	token, err := loadOrCreateToken(secretPath)
	if err != nil {
		return nil, fmt.Errorf("loading auth token: %w", err)
	}

	return &ServerIdentity{
		Certificate: cert,
		CertSHA256:  fingerprint,
		AuthToken:   token,
	}, nil
}

// ServerTLSConfig returns a TLS 1.3-only server configuration.
func ServerTLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
	}
}

// PinnedClientTLSConfig returns a TLS 1.3 client configuration that strictly pins the server certificate's SHA-256 fingerprint.
func PinnedClientTLSConfig(expectedSHA256Hex string) (*tls.Config, error) {
	expected, err := hex.DecodeString(strings.TrimSpace(strings.ToLower(expectedSHA256Hex)))
	if err != nil || len(expected) != sha256.Size {
		return nil, errors.New("invalid SHA-256 certificate fingerprint")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Standard hostname/CA chain checks do not apply to self-signed pinned certs;
		// VerifyPeerCertificate enforces exact cryptographic leaf pinning below.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("server presented no certificate")
			}
			actual := sha256.Sum256(rawCerts[0])
			if subtle.ConstantTimeCompare(actual[:], expected) != 1 {
				return errors.New("TLS certificate fingerprint mismatch")
			}
			return nil
		},
	}, nil
}

// ConstantTimeTokenEqual compares two secret tokens safely.
func ConstantTimeTokenEqual(a, b string) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func generateSelfSignedCert(certPath, keyPath string) error {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}

	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "RemoteControl-Admin",
			Organization: []string{"RemoteControl"},
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return err
	}

	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return err
	}
	return nil
}

func loadOrCreateToken(path string) (string, error) {
	if fileExists(path) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		var sf secretFile
		if err := json.Unmarshal(data, &sf); err == nil && len(sf.AuthToken) >= 32 {
			return sf.AuthToken, nil
		}
	}
	tok, err := RandomHex(32)
	if err != nil {
		return "", err
	}
	raw, err := json.MarshalIndent(secretFile{AuthToken: tok}, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
