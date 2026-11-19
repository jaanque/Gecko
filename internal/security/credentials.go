package security

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// DefaultClientCredentialID identifies the legacy/shared client bundle embedded in RelayProfile.
	DefaultClientCredentialID = "default"
	clientRolePrefix          = "client:"
)

// IssuedClient records metadata and revocation state for a unique client credential.
type IssuedClient struct {
	ID         string    `json:"id"`
	Label      string    `json:"label,omitempty"`
	CertSerial string    `json:"cert_serial"`
	TokenHash  string    `json:"token_hash"`
	Revoked    bool      `json:"revoked"`
	CreatedAt  time.Time `json:"created_at"`
	RevokedAt  time.Time `json:"revoked_at,omitempty"`
}

// IssuedClientCredential holds the newly minted secret material for one ClientRemot binary.
type IssuedClientCredential struct {
	ID            string
	Label         string
	ClientToken   string
	ClientCertDER []byte
	ClientKeyDER  []byte
}

type issuedClientsFile struct {
	Clients []IssuedClient `json:"clients"`
}

func (id *ServerIdentity) issuedClientsPath() string {
	if id.DataDir == "" {
		return ""
	}
	return filepath.Join(id.DataDir, "issued_clients.json")
}

func (id *ServerIdentity) loadIssuedClientsLocked() error {
	id.issuedClients = make(map[string]IssuedClient)
	path := id.issuedClientsPath()
	if path == "" || !fileExists(path) {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var f issuedClientsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	for _, c := range f.Clients {
		if c.ID != "" {
			id.issuedClients[c.ID] = c
		}
	}
	return nil
}

func (id *ServerIdentity) saveIssuedClientsLocked() error {
	path := id.issuedClientsPath()
	if path == "" {
		return nil
	}
	list := make([]IssuedClient, 0, len(id.issuedClients))
	for _, c := range id.issuedClients {
		list = append(list, c)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt.Before(list[j].CreatedAt)
	})
	raw, err := json.MarshalIndent(issuedClientsFile{Clients: list}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// IssueClientCredential mints a unique CA-signed mTLS client certificate (CN="client:<id>")
// and 256-bit auth token, persisting its hash and serial for per-client revocation.
func (id *ServerIdentity) IssueClientCredential(label string) (IssuedClientCredential, error) {
	if id == nil || id.CACert == nil || id.CAKey == nil {
		return IssuedClientCredential{}, errors.New("CA identity not initialized")
	}

	credID, err := RandomHex(6) // 12-char readable ID
	if err != nil {
		return IssuedClientCredential{}, err
	}
	token, err := RandomHex(32)
	if err != nil {
		return IssuedClientCredential{}, err
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return IssuedClientCredential{}, err
	}
	serial, err := randomSerial()
	if err != nil {
		return IssuedClientCredential{}, err
	}

	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   clientRolePrefix + credID,
			Organization: []string{"RemoteControl"},
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, id.CACert, &priv.PublicKey, id.CAKey)
	if err != nil {
		return IssuedClientCredential{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return IssuedClientCredential{}, err
	}

	sum := sha256.Sum256([]byte(token))
	cleanLabel := strings.TrimSpace(label)
	if len(cleanLabel) > 64 {
		cleanLabel = cleanLabel[:64]
	}

	rec := IssuedClient{
		ID:         credID,
		Label:      cleanLabel,
		CertSerial: serial.Text(16),
		TokenHash:  hex.EncodeToString(sum[:]),
		Revoked:    false,
		CreatedAt:  now,
	}

	id.credMu.Lock()
	if id.issuedClients == nil {
		id.issuedClients = make(map[string]IssuedClient)
	}
	id.issuedClients[credID] = rec
	err = id.saveIssuedClientsLocked()
	id.credMu.Unlock()
	if err != nil {
		return IssuedClientCredential{}, fmt.Errorf("saving issued client: %w", err)
	}

	return IssuedClientCredential{
		ID:            credID,
		Label:         cleanLabel,
		ClientToken:   token,
		ClientCertDER: certDER,
		ClientKeyDER:  keyDER,
	}, nil
}

// VerifyClientCredential validates a verified client certificate + auth token against either
// a uniquely issued credential (CN="client:<id>") or the legacy shared client credential (CN="client").
func (id *ServerIdentity) VerifyClientCredential(cert *x509.Certificate, token string) (string, bool) {
	if id == nil || cert == nil || len(token) < 16 {
		return "", false
	}
	cn := cert.Subject.CommonName
	if cn == RoleClient {
		id.credMu.RLock()
		rec, hasDefault := id.issuedClients[DefaultClientCredentialID]
		id.credMu.RUnlock()
		if hasDefault && rec.Revoked {
			return "", false
		}
		if !ConstantTimeTokenEqual(token, id.ClientToken) {
			return "", false
		}
		return DefaultClientCredentialID, true
	}

	if !strings.HasPrefix(cn, clientRolePrefix) {
		return "", false
	}
	credID := strings.TrimPrefix(cn, clientRolePrefix)
	if credID == "" {
		return "", false
	}

	id.credMu.RLock()
	rec, ok := id.issuedClients[credID]
	id.credMu.RUnlock()
	if !ok || rec.Revoked {
		return "", false
	}
	if cert.SerialNumber == nil || !ConstantTimeTokenEqual(cert.SerialNumber.Text(16), rec.CertSerial) {
		return "", false
	}
	sum := sha256.Sum256([]byte(token))
	if !ConstantTimeTokenEqual(hex.EncodeToString(sum[:]), rec.TokenHash) {
		return "", false
	}
	return credID, true
}

// RevokeClient marks a client credential (or "default") as revoked and persists the change.
func (id *ServerIdentity) RevokeClient(credID string) error {
	credID = strings.TrimSpace(credID)
	if credID == "" {
		return errors.New("credential ID vacío")
	}

	id.credMu.Lock()
	defer id.credMu.Unlock()

	if id.issuedClients == nil {
		id.issuedClients = make(map[string]IssuedClient)
	}
	rec, ok := id.issuedClients[credID]
	if !ok {
		if credID == DefaultClientCredentialID {
			rec = IssuedClient{
				ID:        DefaultClientCredentialID,
				Label:     "Credencial compartida inicial",
				CreatedAt: time.Now().UTC(),
			}
		} else {
			return fmt.Errorf("credencial %q no encontrada", credID)
		}
	}
	rec.Revoked = true
	rec.RevokedAt = time.Now().UTC()
	id.issuedClients[credID] = rec
	return id.saveIssuedClientsLocked()
}

// ListIssuedClients returns a snapshot of all tracked client credentials sorted by creation time.
func (id *ServerIdentity) ListIssuedClients() []IssuedClient {
	id.credMu.RLock()
	defer id.credMu.RUnlock()
	out := make([]IssuedClient, 0, len(id.issuedClients))
	for _, c := range id.issuedClients {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}
