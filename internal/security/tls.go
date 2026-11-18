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
	"encoding/base64"
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

const adminKeyPrefix = "RMT1:"

// ServerIdentity holds the persisted Relay TLS certificate, its SHA-256 fingerprint,
// and separate authentication tokens for Admin and Client roles.
type ServerIdentity struct {
	Certificate tls.Certificate
	CertSHA256  string
	AdminToken  string
	ClientToken string
}

// RelayProfile holds everything AdminRemoto.exe needs to connect to the Relay server
// and generate ClienteRemoto.exe binaries.
type RelayProfile struct {
	ServerAddr  string `json:"server_addr,omitempty"`
	CertSHA256  string `json:"cert_sha256"`
	AdminToken  string `json:"admin_token"`
	ClientToken string `json:"client_token"`
}

// Validate checks that all required RelayProfile fields are well-formed.
func (p RelayProfile) Validate() error {
	if strings.TrimSpace(p.ServerAddr) == "" {
		return errors.New("la dirección del servidor es obligatoria")
	}
	if len(strings.TrimSpace(p.CertSHA256)) != 64 {
		return errors.New("la huella TLS SHA-256 debe tener 64 caracteres hexadecimales")
	}
	if len(strings.TrimSpace(p.AdminToken)) < 32 {
		return errors.New("el token de administrador no es válido")
	}
	if len(strings.TrimSpace(p.ClientToken)) < 32 {
		return errors.New("el token de cliente no es válido")
	}
	return nil
}

// EncodeProfileKey packs a RelayProfile into a compact copy-pasteable string ("RMT1:...").
func EncodeProfileKey(p RelayProfile) (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return adminKeyPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// DecodeProfileKey unpacks a "RMT1:..." key into a RelayProfile.
func DecodeProfileKey(key string) (RelayProfile, error) {
	s := strings.TrimSpace(key)
	if !strings.HasPrefix(s, adminKeyPrefix) {
		return RelayProfile{}, errors.New("clave de servidor inválida (debe empezar por RMT1:)")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, adminKeyPrefix))
	if err != nil {
		return RelayProfile{}, errors.New("clave de servidor malformada")
	}
	var p RelayProfile
	if err := json.Unmarshal(raw, &p); err != nil {
		return RelayProfile{}, errors.New("clave de servidor corrupta")
	}
	if len(p.CertSHA256) != 64 || len(p.AdminToken) < 32 || len(p.ClientToken) < 32 {
		return RelayProfile{}, errors.New("la clave no contiene credenciales válidas")
	}
	return p, nil
}

type secretFile struct {
	AdminToken  string `json:"admin_token"`
	ClientToken string `json:"client_token"`
}

// RandomHex returns n random bytes encoded as lowercase hex.
func RandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// LoadOrCreateIdentity loads or creates server.crt, server.key, and relay_secret.json inside dataDir.
func LoadOrCreateIdentity(dataDir string) (*ServerIdentity, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}

	certPath := filepath.Join(dataDir, "server.crt")
	keyPath := filepath.Join(dataDir, "server.key")
	secretPath := filepath.Join(dataDir, "relay_secret.json")

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

	adminTok, clientTok, err := loadOrCreateTokens(secretPath)
	if err != nil {
		return nil, fmt.Errorf("loading relay tokens: %w", err)
	}

	return &ServerIdentity{
		Certificate: cert,
		CertSHA256:  fingerprint,
		AdminToken:  adminTok,
		ClientToken: clientTok,
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
			CommonName:   "RemoteControl-Relay",
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

func loadOrCreateTokens(path string) (string, string, error) {
	if fileExists(path) {
		data, err := os.ReadFile(path)
		if err == nil {
			var sf secretFile
			if json.Unmarshal(data, &sf) == nil && len(sf.AdminToken) >= 32 && len(sf.ClientToken) >= 32 {
				return sf.AdminToken, sf.ClientToken, nil
			}
		}
	}
	adminTok, err := RandomHex(32)
	if err != nil {
		return "", "", err
	}
	clientTok, err := RandomHex(32)
	if err != nil {
		return "", "", err
	}
	raw, err := json.MarshalIndent(secretFile{
		AdminToken:  adminTok,
		ClientToken: clientTok,
	}, "", "  ")
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", "", err
	}
	return adminTok, clientTok, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
