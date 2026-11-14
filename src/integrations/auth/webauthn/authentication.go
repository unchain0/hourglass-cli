package webauthn

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"time"
)

const xsrfTokenHeader = "X-Hourglass-XSRF-Token"

var (
	jsonMarshalAuthentication = json.Marshal
	createAssertionAuthData   = func(a *Authenticator, cred *Credential, clientDataHash []byte) ([]byte, error) {
		return a.createAssertionAuthenticatorData(cred, clientDataHash)
	}
	createAssertionSignature = func(a *Authenticator, privateKey *ecdsa.PrivateKey, authData, clientDataHash []byte) ([]byte, error) {
		return a.createSignature(privateKey, authData, clientDataHash)
	}
	ecdsaSignAuthentication = ecdsa.Sign
)

func (a *Authenticator) Authenticate() (*AuthTokens, error) {
	cred, err := a.getStoredCredential()
	if err != nil {
		return nil, fmt.Errorf("no stored credential found: %w", err)
	}

	beginResp, err := a.beginAuthentication()
	if err != nil {
		fmt.Printf("⚠️  Endpoint de login não disponível (%v)\n", err)
		fmt.Println("ℹ️  Tokens devem ser atualizados manualmente via navegador")
		return nil, fmt.Errorf("autenticação automática indisponível: %w", err)
	}

	assertion, err := a.createAssertion(cred, beginResp)
	if err != nil {
		return nil, fmt.Errorf("create assertion failed: %w", err)
	}

	tokens, err := a.finishAuthentication(assertion)
	if err != nil {
		return nil, fmt.Errorf("finish authentication failed: %w", err)
	}

	cred.incrementSignCount()
	if err := a.updateStoredCredential(cred); err != nil {
		return nil, fmt.Errorf("update credential failed: %w", err)
	}

	return tokens, nil
}

func (a *Authenticator) getStoredCredential() (*Credential, error) {
	storedCreds, err := a.storage.Load()
	if err != nil {
		return nil, err
	}

	if len(storedCreds.Credentials) == 0 {
		return nil, fmt.Errorf("no credentials stored")
	}

	return &storedCreds.Credentials[0], nil
}

func (a *Authenticator) updateStoredCredential(cred *Credential) error {
	storedCreds, err := a.storage.Load()
	if err != nil {
		return err
	}

	for i, c := range storedCreds.Credentials {
		if c.ID == cred.ID {
			storedCreds.Credentials[i] = *cred
			return a.storage.Save(storedCreds)
		}
	}

	return fmt.Errorf("credential not found")
}

type BeginAuthenticationResponse struct {
	PublicKey struct {
		Challenge string `json:"challenge"`
		Timeout   int64  `json:"timeout"`
		RpID      string `json:"rpId"`
	} `json:"publicKey"`
}

func (a *Authenticator) beginAuthentication() (*BeginAuthenticationResponse, error) {
	url := fmt.Sprintf("%s%s/login/begin", a.baseURL, webAuthnAPIPath)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	if a.hgLogin != "" {
		cookie := &http.Cookie{
			Name:  "hglogin",
			Value: a.hgLogin,
		}
		req.AddCookie(cookie)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status: %d, body: %s", resp.StatusCode, string(body))
	}

	var beginResp BeginAuthenticationResponse
	if err := json.NewDecoder(resp.Body).Decode(&beginResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	for _, cookie := range resp.Cookies() {
		switch cookie.Name {
		case "hglogin":
			a.hgLogin = cookie.Value
		case xsrfTokenHeader:
			a.xsrfToken = cookie.Value
		}
	}

	return &beginResp, nil
}

type AssertionResponse struct {
	Type                    string         `json:"type"`
	ID                      string         `json:"id"`
	RawID                   string         `json:"rawId"`
	AuthenticatorAttachment string         `json:"authenticatorAttachment"`
	Response                AssertionData  `json:"response"`
	ClientExtensionResults  map[string]any `json:"clientExtensionResults"`
}

type AssertionData struct {
	AuthenticatorData string `json:"authenticatorData"`
	ClientDataJSON    string `json:"clientDataJSON"`
	Signature         string `json:"signature"`
	UserHandle        string `json:"userHandle"`
}

func (a *Authenticator) createAssertion(cred *Credential, beginResp *BeginAuthenticationResponse) (*AssertionResponse, error) {
	privateKey, err := cred.GetPrivateKey()
	if err != nil {
		return nil, err
	}

	clientData := struct {
		Type        string `json:"type"`
		Challenge   string `json:"challenge"`
		Origin      string `json:"origin"`
		CrossOrigin bool   `json:"crossOrigin"`
	}{
		Type:        "webauthn.get",
		Challenge:   beginResp.PublicKey.Challenge,
		Origin:      fmt.Sprintf("https://app.%s", beginResp.PublicKey.RpID),
		CrossOrigin: false,
	}

	clientDataJSON, err := jsonMarshalAuthentication(clientData)
	if err != nil {
		return nil, fmt.Errorf("marshal client data: %w", err)
	}

	clientDataHash := sha256.Sum256(clientDataJSON)

	authData, err := createAssertionAuthData(a, cred, clientDataHash[:])
	if err != nil {
		return nil, fmt.Errorf("create authenticator data: %w", err)
	}

	signature, err := createAssertionSignature(a, privateKey, authData, clientDataHash[:])
	if err != nil {
		return nil, fmt.Errorf("create signature: %w", err)
	}

	userHandle, err := cred.GetUserIDBytes()
	if err != nil {
		return nil, fmt.Errorf("decode user handle: %w", err)
	}

	return &AssertionResponse{
		Type:                    "public-key",
		ID:                      cred.ID,
		RawID:                   cred.ID,
		AuthenticatorAttachment: "platform",
		Response: AssertionData{
			AuthenticatorData: base64.RawURLEncoding.EncodeToString(authData),
			ClientDataJSON:    base64.RawURLEncoding.EncodeToString(clientDataJSON),
			Signature:         base64.RawURLEncoding.EncodeToString(signature),
			UserHandle:        base64.RawURLEncoding.EncodeToString(userHandle),
		},
		ClientExtensionResults: map[string]any{},
	}, nil
}

func (a *Authenticator) createAssertionAuthenticatorData(cred *Credential, _ []byte) ([]byte, error) {
	rpIDHash := sha256.Sum256([]byte(cred.RPID))

	flags := byte(0x01)

	signCount := make([]byte, 4)
	binary.BigEndian.PutUint32(signCount, cred.SignCount)

	authData := make([]byte, 0, 37)
	authData = append(authData, rpIDHash[:]...)
	authData = append(authData, flags)
	authData = append(authData, signCount...)

	return authData, nil
}

func (a *Authenticator) createSignature(privateKey *ecdsa.PrivateKey, authData, clientDataHash []byte) ([]byte, error) {
	sigData := make([]byte, 0, len(authData)+len(clientDataHash))
	sigData = append(sigData, authData...)
	sigData = append(sigData, clientDataHash...)

	hash := sha256.Sum256(sigData)

	r, s, err := ecdsaSignAuthentication(rand.Reader, privateKey, hash[:])
	if err != nil {
		return nil, fmt.Errorf("sign failed: %w", err)
	}

	return encodeECDSASignature(r, s)
}

func encodeECDSASignature(r, s *big.Int) ([]byte, error) {
	return asn1.Marshal(struct {
		R *big.Int
		S *big.Int
	}{R: r, S: s})
}

// finishRequest builds the authenticated request that completes the ceremony.
func (a *Authenticator) finishRequest(assertion *AssertionResponse) (*http.Request, error) {
	url := fmt.Sprintf("%s%s/login/finish", a.baseURL, webAuthnAPIPath)
	body, err := json.Marshal(assertion)
	if err != nil {
		return nil, fmt.Errorf("marshal assertion: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.xsrfToken != "" {
		req.Header.Set(xsrfTokenHeader, a.xsrfToken)
	}
	if a.hgLogin != "" {
		req.AddCookie(&http.Cookie{Name: "hglogin", Value: a.hgLogin})
	}
	return req, nil
}

func (a *Authenticator) finishAuthentication(assertion *AssertionResponse) (*AuthTokens, error) {
	req, err := a.finishRequest(assertion)
	if err != nil {
		return nil, err
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("authentication failed: status=%d, body=%s", resp.StatusCode, string(body))
	}

	tokens := &AuthTokens{
		ExpiresAt: time.Now().Add(8 * time.Hour),
	}
	var earliestExpiry time.Time

	for _, cookie := range resp.Cookies() {
		switch cookie.Name {
		case "hglogin":
			tokens.HGLogin = cookie.Value
			setHTTPCookieExpiry(cookie, &earliestExpiry)
		case xsrfTokenHeader:
			tokens.XSRFToken = cookie.Value
			setHTTPCookieExpiry(cookie, &earliestExpiry)
		}
	}

	if !earliestExpiry.IsZero() {
		tokens.ExpiresAt = earliestExpiry
	}

	if tokens.HGLogin == "" || tokens.XSRFToken == "" {
		return nil, fmt.Errorf("missing authentication cookies")
	}

	return tokens, nil
}

func setHTTPCookieExpiry(cookie *http.Cookie, earliestExpiry *time.Time) {
	if cookie == nil || cookie.Expires.IsZero() {
		return
	}

	if earliestExpiry.IsZero() || cookie.Expires.Before(*earliestExpiry) {
		*earliestExpiry = cookie.Expires
	}
}
