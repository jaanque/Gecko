package security

import (
	"bytes"
	"compress/zlib"
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
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	adminKeyPrefix = "RMT2:"
	maxProfileRaw  = 64 * 1024

	RoleAdmin  = "admin"
	RoleClient = "client"
)

// ServerIdentity holds the persisted Relay CA, server TLS certificate, mTLS peer certificates
// (Admin and Client), and separate 256-bit authentication tokens.
type ServerIdentity struct {
	Certificate   tls.Certificate
	ClientCAs     *x509.CertPool
	CertSHA256    string
	AdminToken    string
	ClientToken   string
	AdminCertDER  []byte
	AdminKeyDER   []byte
	ClientCertDER []byte
	ClientKeyDER  []byte
}

// RelayProfile holds everything AdminRemoto.exe needs to connect to RelayServidor using mTLS
// and generate ClientRemot binaries with their own mTLS client credentials.
type RelayProfile struct {
	ServerAddr    string `json:"s,omitempty"`
	CertSHA256    string `json:"h"`
	AdminToken    string `json:"at"`
	ClientToken   string `json:"ct"`
	AdminCertDER  []byte `json:"ac"`
	AdminKeyDER   []byte `json:"ak"`
	ClientCertDER []byte `json:"cc"`
	ClientKeyDER  []byte `json:"ck"`
}

// Validate checks that all required RelayProfile fields and mTLS key pairs are valid.
func (p RelayProfile) Validate() error {
	if strings.TrimSpace(p.ServerAddr) == "" {
		return errors.New("la dirección del servidor es obligatoria")
	}
	if _, err := decodeSHA256Hex(p.CertSHA256); err != nil {
		return errors.New("la huella TLS SHA-256 debe tener 64 caracteres hexadecimales")
	}
	if len(strings.TrimSpace(p.AdminToken)) < 32 {
		return errors.New("el token de administrador no es válido")
	}
	if len(strings.TrimSpace(p.ClientToken)) < 32 {
		return errors.New("el token de cliente no es válido")
	}
	if _, err := ParseDERKeyPair(p.AdminCertDER, p.AdminKeyDER); err != nil {
		return errors.New("certificado mTLS de administrador inválido")
	}
	if _, err := ParseDERKeyPair(p.ClientCertDER, p.ClientKeyDER); err != nil {
		return errors.New("certificado mTLS de cliente inválido")
	}
	return nil
}

// EncodeProfileKey packs a RelayProfile into a compact zlib+base64url string ("RMT2:...").
func EncodeProfileKey(p RelayProfile) (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	zw, err := zlib.NewWriterLevel(&buf, zlib.BestCompression)
	if err != nil {
		return "", err
	}
	if _, err := zw.Write(raw); err != nil {
		_ = zw.Close()
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	return adminKeyPrefix + base64.RawURLEncoding.EncodeToString(buf.Bytes()), nil
}

// DecodeProfileKey unpacks a "RMT2:..." key into a RelayProfile.
func DecodeProfileKey(key string) (RelayProfile, error) {
	s := strings.TrimSpace(key)
	if !strings.HasPrefix(s, adminKeyPrefix) {
		return RelayProfile{}, errors.New("clave de servidor inválida (debe empezar por RMT2:)")
	}
	comp, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, adminKeyPrefix))
	if err != nil || len(comp) == 0 || len(comp) > maxProfileRaw {
		return RelayProfile{}, errors.New("clave de servidor malformada")
	}
	zr, err := zlib.NewReader(bytes.NewReader(comp))
	if err != nil {
		return RelayProfile{}, errors.New("clave de servidor corrupta")
	}
	defer zr.Close()

	raw, err := io.ReadAll(io.LimitReader(zr, maxProfileRaw+1))
	if err != nil || len(raw) > maxProfileRaw {
		return RelayProfile{}, errors.New("clave de servidor corrupta")
	}

	var p RelayProfile
	if err := json.Unmarshal(raw, &p); err != nil {
		return RelayProfile{}, errors.New("clave de servidor corrupta")
	}
	if _, err := decodeSHA256Hex(p.CertSHA256); err != nil || len(p.AdminToken) < 32 || len(p.ClientToken) < 32 {
		return RelayProfile{}, errors.New("la clave no contiene credenciales válidas")
	}
	if _, err := ParseDERKeyPair(p.AdminCertDER, p.AdminKeyDER); err != nil {
		return RelayProfile{}, errors.New("la clave no contiene certificado mTLS de administrador válido")
	}
	if _, err := ParseDERKeyPair(p.ClientCertDER, p.ClientKeyDER); err != nil {
		return RelayProfile{}, errors.New("la clave no contiene certificado mTLS de cliente válido")
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

// LoadOrCreateIdentity loads or creates the Relay CA, server cert, mTLS admin/client certs,
// and role tokens inside dataDir.
func LoadOrCreateIdentity(dataDir string) (*ServerIdentity, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}

	caCertPath := filepath.Join(dataDir, "ca.crt")
	caKeyPath := filepath.Join(dataDir, "ca.key")
	srvCertPath := filepath.Join(dataDir, "server.crt")
	srvKeyPath := filepath.Join(dataDir, "server.key")
	admCertPath := filepath.Join(dataDir, "admin.crt")
	admKeyPath := filepath.Join(dataDir, "admin.key")
	cliCertPath := filepath.Join(dataDir, "client.crt")
	cliKeyPath := filepath.Join(dataDir, "client.key")
	secretPath := filepath.Join(dataDir, "relay_secret.json")

	// If any mTLS PKI artifact is missing, regenerate the complete consistent PKI bundle.
	if !fileExists(caCertPath) || !fileExists(caKeyPath) ||
		!fileExists(srvCertPath) || !fileExists(srvKeyPath) ||
		!fileExists(admCertPath) || !fileExists(admKeyPath) ||
		!fileExists(cliCertPath) || !fileExists(cliKeyPath) {
		if err := generateMTLSBundle(caCertPath, caKeyPath, srvCertPath, srvKeyPath, admCertPath, admKeyPath, cliCertPath, cliKeyPath); err != nil {
			return nil, fmt.Errorf("generating mTLS PKI: %w", err)
		}
	}

	caPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("reading CA cert: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("invalid CA certificate PEM")
	}

	srvCert, err := tls.LoadX509KeyPair(srvCertPath, srvKeyPath)
	if err != nil {
		return nil, fmt.Errorf("loading server TLS key pair: %w", err)
	}
	if len(srvCert.Certificate) == 0 {
		return nil, errors.New("empty server certificate chain")
	}
	sum := sha256.Sum256(srvCert.Certificate[0])
	fingerprint := hex.EncodeToString(sum[:])

	admCertDER, admKeyDER, err := readPEMKeyPairDER(admCertPath, admKeyPath)
	if err != nil {
		return nil, fmt.Errorf("loading admin mTLS key pair: %w", err)
	}
	cliCertDER, cliKeyDER, err := readPEMKeyPairDER(cliCertPath, cliKeyPath)
	if err != nil {
		return nil, fmt.Errorf("loading client mTLS key pair: %w", err)
	}

	adminTok, clientTok, err := loadOrCreateTokens(secretPath)
	if err != nil {
		return nil, fmt.Errorf("loading relay tokens: %w", err)
	}

	return &ServerIdentity{
		Certificate:   srvCert,
		ClientCAs:     caPool,
		CertSHA256:    fingerprint,
		AdminToken:    adminTok,
		ClientToken:   clientTok,
		AdminCertDER:  admCertDER,
		AdminKeyDER:   admKeyDER,
		ClientCertDER: cliCertDER,
		ClientKeyDER:  cliKeyDER,
	}, nil
}

// ServerTLSConfig returns a TLS 1.3-only Mutual TLS (mTLS) server configuration that
// strictly requires and verifies a CA-signed client certificate during the TLS handshake.
func ServerTLSConfig(cert tls.Certificate, clientCAs *x509.CertPool) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
	}
}

// MutualPinnedClientTLSConfig returns a TLS 1.3 mTLS client configuration that:
//  1. Presents the client's signed X.509 certificate + private key to the Relay server, and
//  2. Strictly pins the Relay server's leaf certificate SHA-256 fingerprint in constant time.
func MutualPinnedClientTLSConfig(expectedSHA256Hex string, clientCertDER, clientKeyDER []byte) (*tls.Config, error) {
	expected, err := decodeSHA256Hex(expectedSHA256Hex)
	if err != nil {
		return nil, err
	}
	clientCert, err := ParseDERKeyPair(clientCertDER, clientKeyDER)
	if err != nil {
		return nil, fmt.Errorf("invalid mTLS client credentials: %w", err)
	}

	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{clientCert},
		// Self-signed/private CA leaf pinning is enforced cryptographically below.
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

// VerifyServerTLSConn performs an explicit post-handshake verification of a live *tls.Conn
// confirming TLS 1.3 completion and exact constant-time SHA-256 match of the peer certificate.
func VerifyServerTLSConn(conn *tls.Conn, expectedSHA256Hex string) error {
	if conn == nil {
		return errors.New("nil TLS connection")
	}
	expected, err := decodeSHA256Hex(expectedSHA256Hex)
	if err != nil {
		return err
	}
	st := conn.ConnectionState()
	if !st.HandshakeComplete {
		return errors.New("TLS handshake not complete")
	}
	if st.Version != tls.VersionTLS13 {
		return errors.New("TLS 1.3 required")
	}
	if len(st.PeerCertificates) == 0 {
		return errors.New("missing server certificate")
	}
	actual := sha256.Sum256(st.PeerCertificates[0].Raw)
	if subtle.ConstantTimeCompare(actual[:], expected) != 1 {
		return errors.New("server certificate fingerprint mismatch")
	}
	return nil
}

// PeerRoleFromTLS inspects a live server-side mTLS connection and returns the verified
// certificate CommonName ("admin" or "client"). Returns an error if mTLS was not verified.
func PeerRoleFromTLS(rawConn net.Conn) (string, error) {
	tlsConn, ok := rawConn.(*tls.Conn)
	if !ok {
		return "", errors.New("TLS connection required")
	}
	st := tlsConn.ConnectionState()
	if !st.HandshakeComplete || st.Version != tls.VersionTLS13 {
		return "", errors.New("TLS 1.3 mTLS handshake required")
	}
	if len(st.VerifiedChains) == 0 || len(st.VerifiedChains[0]) == 0 {
		return "", errors.New("verified client certificate required")
	}
	role := st.VerifiedChains[0][0].Subject.CommonName
	if role != RoleAdmin && role != RoleClient {
		return "", fmt.Errorf("unknown mTLS client role: %q", role)
	}
	return role, nil
}

// ParseDERKeyPair reconstructs a tls.Certificate from DER-encoded certificate and EC private key bytes.
func ParseDERKeyPair(certDER, keyDER []byte) (tls.Certificate, error) {
	if len(certDER) == 0 || len(keyDER) == 0 {
		return tls.Certificate{}, errors.New("empty certificate or key")
	}
	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		return tls.Certificate{}, err
	}
	priv, err := x509.ParseECPrivateKey(keyDER)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  priv,
		Leaf:        leaf,
	}, nil
}

// ConstantTimeTokenEqual compares two secret tokens safely.
func ConstantTimeTokenEqual(a, b string) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func decodeSHA256Hex(s string) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimSpace(strings.ToLower(s)))
	if err != nil || len(b) != sha256.Size {
		return nil, errors.New("invalid SHA-256 certificate fingerprint")
	}
	return b, nil
}

func generateMTLSBundle(caCertPath, caKeyPath, srvCertPath, srvKeyPath, admCertPath, admKeyPath, cliCertPath, cliKeyPath string) error {
	caPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	caSerial, err := randomSerial()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	caTmpl := &x509.Certificate{
		SerialNumber: caSerial,
		Subject: pkix.Name{
			CommonName:   "RemoteControl-CA",
			Organization: []string{"RemoteControl"},
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caPriv.PublicKey, caPriv)
	if err != nil {
		return err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}
	if err := writeKeyPairPEM(caCertPath, caKeyPath, caDER, caPriv); err != nil {
		return err
	}

	// Server leaf cert (ExtKeyUsageServerAuth).
	if err := generateSignedLeaf(caCert, caPriv, "RemoteControl-Relay", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, srvCertPath, srvKeyPath); err != nil {
		return err
	}
	// Admin mTLS client cert (CN = "admin", ExtKeyUsageClientAuth).
	if err := generateSignedLeaf(caCert, caPriv, RoleAdmin, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, admCertPath, admKeyPath); err != nil {
		return err
	}
	// Client mTLS client cert (CN = "client", ExtKeyUsageClientAuth).
	if err := generateSignedLeaf(caCert, caPriv, RoleClient, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, cliCertPath, cliKeyPath); err != nil {
		return err
	}
	return nil
}

func generateSignedLeaf(caCert *x509.Certificate, caPriv *ecdsa.PrivateKey, commonName string, eku []x509.ExtKeyUsage, certPath, keyPath string) error {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := randomSerial()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   commonName,
			Organization: []string{"RemoteControl"},
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           eku,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &priv.PublicKey, caPriv)
	if err != nil {
		return err
	}
	return writeKeyPairPEM(certPath, keyPath, der, priv)
}

func writeKeyPairPEM(certPath, keyPath string, certDER []byte, priv *ecdsa.PrivateKey) error {
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return err
	}
	return os.WriteFile(keyPath, keyPEM, 0o600)
}

func readPEMKeyPairDER(certPath, keyPath string) ([]byte, []byte, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	cBlock, _ := pem.Decode(certPEM)
	if cBlock == nil {
		return nil, nil, errors.New("invalid certificate PEM")
	}
	kBlock, _ := pem.Decode(keyPEM)
	if kBlock == nil {
		return nil, nil, errors.New("invalid private key PEM")
	}
	if _, err := ParseDERKeyPair(cBlock.Bytes, kBlock.Bytes); err != nil {
		return nil, nil, err
	}
	return cBlock.Bytes, kBlock.Bytes, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
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
