package admin

import (
	"errors"
	"strings"
	"time"

	"remote-control/internal/config"
	"remote-control/internal/protocol"
	"remote-control/internal/security"
)

// ProvisionClientCredential asks the live Relay to mint a unique, individually revocable
// mTLS certificate + token for a new client binary.
func (c *RelayClient) ProvisionClientCredential(label string) (protocol.AdminProvisionRespPayload, error) {
	conn := c.currentConn()
	if conn == nil {
		return protocol.AdminProvisionRespPayload{}, errors.New("sin conexión al servidor puente")
	}

	ch := make(chan protocol.AdminProvisionRespPayload, 1)
	c.provMu.Lock()
	c.provCh = ch
	c.provMu.Unlock()
	defer func() {
		c.provMu.Lock()
		if c.provCh == ch {
			c.provCh = nil
		}
		c.provMu.Unlock()
	}()

	if err := c.writeJSON(conn, protocol.MsgAdminProvisionClient, protocol.AdminProvisionReqPayload{
		Label: strings.TrimSpace(label),
	}); err != nil {
		return protocol.AdminProvisionRespPayload{}, err
	}

	select {
	case resp := <-ch:
		if !resp.OK {
			if resp.Error == "" {
				resp.Error = "el servidor rechazó la emisión de credencial"
			}
			return protocol.AdminProvisionRespPayload{}, errors.New(resp.Error)
		}
		return resp, nil
	case <-time.After(5 * time.Second):
		return protocol.AdminProvisionRespPayload{}, errors.New("tiempo de espera agotado al emitir credencial")
	}
}

// GenerateClientBinary creates a client executable bound to the configured Relay server,
// with optional logo, credential label, and UI language ("ca"|"es"|"en").
// When connected to the Relay, it provisions a unique revocable client mTLS certificate and token;
// if offline, it falls back to the shared client credentials stored in RelayProfile (never AdminToken).
func (c *RelayClient) GenerateClientBinary(template, logoPNG []byte, label, lang string) ([]byte, string, error) {
	if len(template) == 0 {
		return nil, "", errors.New("plantilla de cliente no disponible; compila el proyecto con: go run ./cmd/build")
	}
	c.mu.RLock()
	prof := c.profile
	ok := c.hasProfile
	c.mu.RUnlock()

	if !ok {
		return nil, "", errors.New("configura el servidor primero")
	}

	clientTok := prof.ClientToken
	clientCert := prof.ClientCertDER
	clientKey := prof.ClientKeyDER
	credID := security.DefaultClientCredentialID

	if prov, err := c.ProvisionClientCredential(label); err == nil &&
		len(prov.ClientToken) >= 32 && len(prov.ClientCertDER) > 0 && len(prov.ClientKeyDER) > 0 {
		clientTok = prov.ClientToken
		clientCert = prov.ClientCertDER
		clientKey = prov.ClientKeyDER
		credID = prov.CredentialID
	}

	built, err := config.AppendToBinary(template, config.ClientConfig{
		ServerAddr:    prof.ServerAddr,
		CertSHA256:    prof.CertSHA256,
		AuthToken:     clientTok,
		ClientCertDER: clientCert,
		ClientKeyDER:  clientKey,
		LogoPNG:       logoPNG,
		Lang:          strings.TrimSpace(lang),
	})
	return built, credID, err
}

// RevokeClient revokes a client's credentials on the Relay and disconnects it immediately.
func (c *RelayClient) RevokeClient(credentialID, clientID string) error {
	conn := c.currentConn()
	if conn == nil {
		return errors.New("no hay conexión con el servidor puente")
	}
	return c.writeJSON(conn, protocol.MsgAdminRevokeClient, protocol.AdminRevokeClientPayload{
		CredentialID: strings.TrimSpace(credentialID),
		ClientID:     strings.TrimSpace(clientID),
	})
}
