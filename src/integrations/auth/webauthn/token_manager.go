package webauthn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type renewalTicker interface {
	C() <-chan time.Time
	Stop()
}

type timeTicker struct {
	*time.Ticker
}

func (t *timeTicker) C() <-chan time.Time {
	return t.Ticker.C
}

var (
	osUserHomeDirTokenManager = os.UserHomeDir
	osMkdirAllTokenManager    = os.MkdirAll
	osWriteFileTokenManager   = os.WriteFile
	osRenameTokenManager      = os.Rename
	osRemoveTokenManager      = os.Remove
	osStatTokenManager        = os.Stat
	osReadFileTokenManager    = os.ReadFile
	jsonMarshalTokenManager   = json.Marshal
	jsonUnmarshalTokenManager = json.Unmarshal
	newRenewalTicker          = func(d time.Duration) renewalTicker {
		return &timeTicker{Ticker: time.NewTicker(d)}
	}
	authenticatorAuthenticate = func(a *Authenticator) (*AuthTokens, error) {
		return a.Authenticate()
	}
	authenticatorSetCookies = func(a *Authenticator, xsrfToken, hgLogin string) {
		a.SetCookies(xsrfToken, hgLogin)
	}
	browserAuthenticate = func(b *BrowserAuth) (*AuthTokens, error) {
		return b.Authenticate()
	}
)

// TokenManager handles automatic token renewal using WebAuthn.
type TokenManager struct {
	authenticator     *Authenticator
	browserAuth       *BrowserAuth
	storagePath       string
	tokensPath        string
	baseURL           string
	browserProfileDir string

	currentTokens *AuthTokens
	mu            sync.RWMutex
	renewMu       sync.Mutex

	// Callbacks
	onTokenRenewed func(tokens *AuthTokens)
	onError        func(err error)

	// Configuration
	renewalThreshold time.Duration
	stopChan         chan struct{}
	stopOnce         sync.Once
}

// TokenManagerOption configures the TokenManager.
type TokenManagerOption func(*TokenManager)

// WithOnTokenRenewed sets a callback for when tokens are renewed.
func WithOnTokenRenewed(callback func(tokens *AuthTokens)) TokenManagerOption {
	return func(tm *TokenManager) {
		tm.onTokenRenewed = callback
	}
}

// WithOnError sets a callback for errors.
func WithOnError(callback func(err error)) TokenManagerOption {
	return func(tm *TokenManager) {
		tm.onError = callback
	}
}

// WithRenewalThreshold sets how close to expiry before renewing (default: 1 hour).
func WithRenewalThreshold(threshold time.Duration) TokenManagerOption {
	return func(tm *TokenManager) {
		tm.renewalThreshold = threshold
	}
}

func WithBrowserAuth(browserAuth *BrowserAuth) TokenManagerOption {
	return func(tm *TokenManager) {
		tm.browserAuth = browserAuth
	}
}

func WithBrowserProfileDir(path string) TokenManagerOption {
	return func(tm *TokenManager) {
		if path == "" {
			return
		}

		tm.browserProfileDir = path
		if tm.browserAuth == nil {
			tm.browserAuth = NewBrowserAuth(tm.baseURL).WithProfileDir(path)
			return
		}

		tm.browserAuth = tm.browserAuth.WithProfileDir(path)
	}
}

// WithTokensPath sets the file path used to persist auth tokens.
func WithTokensPath(path string) TokenManagerOption {
	return func(tm *TokenManager) {
		if path != "" {
			tm.tokensPath = path
		}
	}
}

// NewTokenManager creates a new token manager for automatic renewal.
func NewTokenManager(storagePath, baseURL string, opts ...TokenManagerOption) (*TokenManager, error) {
	storagePath = resolveStoragePath(storagePath)
	tokensPath := resolveTokensPath(storagePath)
	browserProfileDir := os.Getenv("CHROME_PROFILE_DIR")
	baseURL = normalizeWebAuthnBaseURL(baseURL)

	authenticator, err := NewAuthenticator(storagePath, baseURL)
	if err != nil {
		return nil, err
	}

	isHeadless := IsHeadlessEnvironment()
	if isHeadless {
		slog.Info("detected headless environment, disabling browser authentication", "storage_path", storagePath)
	}

	tm := &TokenManager{
		authenticator:     authenticator,
		storagePath:       storagePath,
		tokensPath:        tokensPath,
		baseURL:           baseURL,
		browserProfileDir: browserProfileDir,
		renewalThreshold:  1 * time.Hour,
		stopChan:          make(chan struct{}),
	}

	if !isHeadless || browserProfileDir != "" {
		tm.browserAuth = NewBrowserAuth(baseURL)
		if browserProfileDir != "" {
			tm.browserAuth = tm.browserAuth.WithProfileDir(browserProfileDir)
		}
	}

	for _, opt := range opts {
		opt(tm)
	}

	slog.Info("token manager initialized", "tokens_path", tm.tokensPath, "storage_path", tm.storagePath, "headless", isHeadless, "has_browser_auth", tm.browserAuth != nil, "browser_profile_dir", tm.browserProfileDir)

	return tm, nil
}

// Start begins the automatic token renewal loop.
func (tm *TokenManager) Start(ctx context.Context) error {
	loadedTokens, err := tm.LoadTokens()
	if err != nil {
		return fmt.Errorf("failed to load persisted tokens: %w", err)
	}

	if loadedTokens != nil {
		slog.Info("loaded persisted authentication tokens", "path", tm.tokensPath, "expires_at", loadedTokens.ExpiresAt)
		tm.setTokens(loadedTokens)
	}

	tokens, err := tm.EnsureValidTokens()
	if err != nil {
		return err
	}

	if loadedTokens != nil && loadedTokens.IsUsable() && !loadedTokens.IsNearExpiry(tm.renewalThreshold) && tm.onTokenRenewed != nil {
		tm.onTokenRenewed(tokens)
	}

	// Start renewal loop
	go tm.renewalLoop(ctx)

	return nil
}

// Stop stops the automatic renewal loop.
func (tm *TokenManager) Stop() {
	tm.stopOnce.Do(func() {
		close(tm.stopChan)
	})
}

// GetTokens returns the current authentication tokens.
func (tm *TokenManager) GetTokens() *AuthTokens {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	if tm.currentTokens == nil {
		return nil
	}

	// Return copy
	return &AuthTokens{
		HGLogin:   tm.currentTokens.HGLogin,
		XSRFToken: tm.currentTokens.XSRFToken,
		ExpiresAt: tm.currentTokens.ExpiresAt,
	}
}

// IsAuthenticated returns true if we have valid tokens.
func (tm *TokenManager) IsAuthenticated() bool {
	tokens := tm.GetTokens()
	return tokens != nil && tokens.IsUsable() && !tokens.IsExpired()
}

// EnsureValidTokens ensures tokens are valid, renewing if necessary.
func (tm *TokenManager) EnsureValidTokens() (*AuthTokens, error) {
	tokens := tm.GetTokens()

	if tokens != nil {
		timeUntilExpiry := time.Until(tokens.ExpiresAt)
		slog.Debug("checking token validity", "expires_at", tokens.ExpiresAt, "time_until_expiry", timeUntilExpiry, "threshold", tm.renewalThreshold)
	}

	if !tm.tokensNeedRenewal(tokens) {
		slog.Debug("tokens still valid, no renewal needed", "expires_at", tokens.ExpiresAt)
		return tokens, nil
	}

	return tm.renewTokens(false)
}

// ForceRenewal bypasses the expiry threshold and performs a fresh authentication round.
func (tm *TokenManager) ForceRenewal() (*AuthTokens, error) {
	return tm.renewTokens(true)
}

func (tm *TokenManager) tokensNeedRenewal(tokens *AuthTokens) bool {
	if tokens == nil {
		return true
	}

	if !tokens.IsUsable() {
		return true
	}

	return tokens.IsNearExpiry(tm.renewalThreshold)
}

func (tm *TokenManager) renewTokens(force bool) (*AuthTokens, error) {
	tm.renewMu.Lock()
	defer tm.renewMu.Unlock()

	tokens := tm.GetTokens()
	if !force && !tm.tokensNeedRenewal(tokens) {
		slog.Debug("tokens refreshed by another goroutine", "expires_at", tokens.ExpiresAt)
		return tokens, nil
	}

	switch {
	case force:
		slog.Warn("forcing authentication token renewal")
	case tokens == nil:
		slog.Info("no tokens available, attempting authentication")
	case !tokens.IsUsable():
		slog.Warn("persisted tokens are incomplete, re-authenticating")
	default:
		slog.Info("tokens near expiry or expired, renewing", "expires_at", tokens.ExpiresAt, "threshold", tm.renewalThreshold)
	}

	newTokens, err := tm.authenticateWithCurrentTokens(tokens)
	if err != nil {
		slog.Error("authentication failed", "error", err)
		if tm.onError != nil {
			tm.onError(err)
		}
		return nil, err
	}

	slog.Info("authentication successful, updating tokens", "expires_at", newTokens.ExpiresAt)
	tm.setTokens(newTokens)

	if err := tm.SaveTokens(newTokens); err != nil {
		slog.Error("CRITICAL: failed to persist authentication tokens", "path", tm.tokensPath, "error", err)
	} else {
		slog.Info("tokens persisted successfully", "path", tm.tokensPath)
	}

	if tm.onTokenRenewed != nil {
		slog.Info("calling onTokenRenewed callback")
		tm.onTokenRenewed(newTokens)
	}

	return newTokens, nil
}

// resolveStoragePath picks the credentials path, falling back to the default directory.
func resolveStoragePath(storagePath string) string {
	if storagePath != "" {
		return storagePath
	}
	if fromEnv := os.Getenv("WEBAUTHN_CREDENTIALS_PATH"); fromEnv != "" {
		return fromEnv
	}
	homeDir, err := osUserHomeDirTokenManager()
	if err != nil {
		return storagePath
	}
	return filepath.Join(homeDir, ".hourglass-rpa", "webauthn-credentials.json")
}

func resolveTokensPath(storagePath string) string {
	if fromEnv := os.Getenv("WEBAUTHN_TOKENS_PATH"); fromEnv != "" {
		return fromEnv
	}
	return filepath.Join(filepath.Dir(storagePath), "auth-tokens.json")
}

func normalizeWebAuthnBaseURL(baseURL string) string {
	trimmed := strings.TrimRight(baseURL, "/")
	if trimmed == "" {
		return trimmed
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return trimmed
	}

	if strings.HasPrefix(strings.TrimRight(parsed.Path, "/"), "/api/") {
		parsed.Path = ""
		parsed.RawPath = ""
	}

	return strings.TrimRight(parsed.String(), "/")
}

// SaveTokens persists authentication tokens to disk with owner-only permissions.
func (tm *TokenManager) SaveTokens(tokens *AuthTokens) error {
	if tokens == nil {
		return errors.New("tokens cannot be nil")
	}

	if tm.tokensPath == "" {
		return errors.New("tokens path is not configured")
	}

	slog.Info("saving tokens to disk", "path", tm.tokensPath, "expires_at", tokens.ExpiresAt, "hglogin_present", tokens.HGLogin != "", "xsrf_present", tokens.XSRFToken != "")

	data, err := jsonMarshalTokenManager(tokens)
	if err != nil {
		return fmt.Errorf("failed to marshal tokens: %w", err)
	}

	dir := filepath.Dir(tm.tokensPath)
	slog.Debug("ensuring directory exists", "dir", dir)
	if err := osMkdirAllTokenManager(dir, 0o700); err != nil {
		return fmt.Errorf("failed to create tokens directory: %w", err)
	}

	tempPath := tm.tokensPath + ".tmp"
	slog.Debug("writing to temp file", "temp_path", tempPath)
	if err := osWriteFileTokenManager(tempPath, data, 0o600); err != nil {
		return fmt.Errorf("failed to write temp tokens file: %w", err)
	}

	slog.Debug("renaming temp file to final path", "from", tempPath, "to", tm.tokensPath)
	if err := osRenameTokenManager(tempPath, tm.tokensPath); err != nil {
		_ = osRemoveTokenManager(tempPath)
		return fmt.Errorf("failed to rename tokens file: %w", err)
	}

	info, err := osStatTokenManager(tm.tokensPath)
	if err != nil {
		slog.Warn("could not stat tokens file after save", "error", err)
	} else {
		slog.Info("tokens saved successfully", "path", tm.tokensPath, "size", info.Size(), "expires_at", tokens.ExpiresAt)
	}

	return nil
}

// LoadTokens loads persisted authentication tokens from disk.
func (tm *TokenManager) LoadTokens() (*AuthTokens, error) {
	if tm.tokensPath == "" {
		return nil, errors.New("tokens path is not configured")
	}

	data, err := osReadFileTokenManager(tm.tokensPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Info("persisted tokens file not found", "path", tm.tokensPath)
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read tokens file: %w", err)
	}

	var tokens AuthTokens
	if err := jsonUnmarshalTokenManager(data, &tokens); err != nil {
		return nil, fmt.Errorf("failed to unmarshal tokens file: %w", err)
	}

	return &tokens, nil
}

func (tm *TokenManager) authenticateWithFallback(currentTokens ...*AuthTokens) (*AuthTokens, error) {
	if len(currentTokens) > 0 {
		return tm.authenticateWithCurrentTokens(currentTokens[0])
	}

	return tm.authenticateWithCurrentTokens(tm.GetTokens())
}

func (tm *TokenManager) authenticateWithCurrentTokens(currentTokens *AuthTokens) (*AuthTokens, error) {
	hasCredentials := tm.HasWebAuthnCredentials()
	slog.Info("attempting authentication fallback", "has_browser_auth", tm.browserAuth != nil, "has_credentials", hasCredentials, "prefer_webauthn", hasCredentials)
	if err := tm.headlessProfileProblem(hasCredentials); err != nil {
		return nil, err
	}
	if currentTokens != nil {
		authenticatorSetCookies(tm.authenticator, currentTokens.XSRFToken, currentTokens.HGLogin)
	}
	if hasCredentials {
		return tm.webAuthnFirst(hasCredentials)
	}
	return tm.browserFirst(hasCredentials)
}

// headlessProfileProblem rejects a headless run whose browser profile has no cookie store.
func (tm *TokenManager) headlessProfileProblem(hasCredentials bool) error {
	if !IsHeadlessEnvironment() || hasCredentials || tm.browserAuth == nil || tm.browserProfileDir == "" {
		return nil
	}
	if browserProfileHasCookieStore(tm.browserProfileDir) {
		return nil
	}
	return fmt.Errorf("persistent browser profile has no cookie store at %s; run setup-auth and import the generated authentication files before starting the headless service", tm.browserProfileDir)
}

func (tm *TokenManager) webAuthnFirst(hasCredentials bool) (*AuthTokens, error) {
	tokens, err := tm.authenticateWithWebAuthn(hasCredentials)
	if err == nil {
		return tokens, nil
	}
	if tm.browserAuth == nil {
		return nil, err
	}
	slog.Warn("WebAuthn authentication failed, falling back to browser profile auth", "error", err)
	return tm.authenticateWithBrowser()
}

func (tm *TokenManager) browserFirst(hasCredentials bool) (*AuthTokens, error) {
	if tm.browserAuth == nil {
		slog.Info("browser auth disabled (headless environment), using WebAuthn only")
		return tm.authenticateWithWebAuthn(hasCredentials)
	}
	tokens, err := tm.authenticateWithBrowser()
	if err == nil {
		return tokens, nil
	}
	slog.Error("browser authentication failed, falling back to WebAuthn", "error", err)
	return tm.authenticateWithWebAuthn(hasCredentials)
}

func browserProfileHasCookieStore(profileDir string) bool {
	entries, err := os.ReadDir(profileDir)
	if err != nil {
		return false
	}

	for _, entry := range entries {
		if !entry.IsDir() || (entry.Name() != "Default" && !strings.HasPrefix(entry.Name(), "Profile ")) {
			continue
		}

		for _, relativePath := range []string{"Cookies", filepath.Join("Network", "Cookies")} {
			info, err := os.Stat(filepath.Join(profileDir, entry.Name(), relativePath))
			if err == nil && info.Mode().IsRegular() && info.Size() > 0 {
				return true
			}
		}
	}

	return false
}

func (tm *TokenManager) authenticateWithBrowser() (*AuthTokens, error) {
	slog.Info("trying browser authentication")
	tokens, err := browserAuthenticate(tm.browserAuth)
	if err != nil {
		return nil, err
	}

	slog.Info("browser authentication succeeded")
	return tokens, nil
}

func (tm *TokenManager) authenticateWithWebAuthn(hasCredentials bool) (*AuthTokens, error) {
	if !hasCredentials {
		slog.Error("no WebAuthn credentials found", "storage_path", tm.storagePath)
		return nil, fmt.Errorf("no WebAuthn credentials available - run 'make setup-auth' to configure")
	}

	slog.Info("attempting WebAuthn authentication", "storage_path", tm.storagePath)
	tokens, err := authenticatorAuthenticate(tm.authenticator)
	if err != nil {
		slog.Error("WebAuthn authentication failed", "error", err)
		return nil, fmt.Errorf("browser auth fallback failed: %w", err)
	}

	slog.Info("WebAuthn authentication succeeded")
	return tokens, nil
}

func (tm *TokenManager) setTokens(tokens *AuthTokens) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.currentTokens = tokens
}

func (tm *TokenManager) PrimeTokens(tokens *AuthTokens) {
	tm.setTokens(tokens)
}

func (tm *TokenManager) renewalLoop(ctx context.Context) {
	ticker := newRenewalTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tm.stopChan:
			return
		case <-ticker.C():
			_, err := tm.EnsureValidTokens()
			if err != nil {
				slog.Error("failed to renew tokens", "error", err)
			}
		}
	}
}

// GetHGLogin returns the current hglogin cookie value.
func (tm *TokenManager) GetHGLogin() string {
	tokens := tm.GetTokens()
	if tokens == nil {
		return ""
	}
	return tokens.HGLogin
}

// GetXSRFToken returns the current XSRF token.
func (tm *TokenManager) GetXSRFToken() string {
	tokens := tm.GetTokens()
	if tokens == nil {
		return ""
	}
	return tokens.XSRFToken
}
