// Package main provides a command-line tool for saving authentication tokens.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"hourglass-rejections-rpa/src/integrations/auth/webauthn"
)

var (
	chromeStatFn           = os.Stat
	prepareChromeProfileFn = webauthn.PrepareChromeProfile
	execCommandFn          = exec.Command
)

const (
	appBaseURL            = "https://app.hourglass-app.com"
	loginPagePath         = "/v2/page/app"
	defaultConfigDirName  = ".hourglass-rpa"
	authTokensFileName    = "auth-tokens.json"
	webauthnFileName      = "webauthn-credentials.json"
	vpsTokensPathTemplate = "~/.hourglass-rpa/"
	tokenDateTimeFormat   = "02/01/2006 15:04:05"
)

var osExit = os.Exit

type tokenLoader interface {
	LoadTokens() (*webauthn.AuthTokens, error)
}

var newTokenSaverForMain = newTokenSaver

var userHomeDirForMain = os.UserHomeDir

func defaultNewTokenLoader(configDir, tokensPath string) (tokenLoader, error) {
	return webauthn.NewTokenManager(
		filepath.Join(configDir, webauthnFileName),
		appBaseURL,
		webauthn.WithTokensPath(tokensPath),
	)
}

var newTokenLoader = defaultNewTokenLoader

var logFatal = log.Fatal

var printSuccessFn = printSuccess

func printTokenRenewedMessage() {
	fmt.Println("🔄 Tokens renovados!")
}

func onTokenRenewed(_ *webauthn.AuthTokens) {
	printTokenRenewedMessage()
}

type tokenSaver interface {
	SaveTokens(tokens *webauthn.AuthTokens) error
}

type browserAuthenticator interface {
	Authenticate() (*webauthn.AuthTokens, error)
	ExtractTokensFromProfile() (*webauthn.AuthTokens, error)
	WithHeadless(headless bool) browserAuthenticator
	WithProfileDir(profileDir string) browserAuthenticator
}

type tokenSaverImpl struct {
	tokenManagerFactory func(credsPath, baseURL string, opts ...webauthn.TokenManagerOption) (tokenSaver, error)
	browserAuthFactory  func(baseURL string) browserAuthenticator
	userHomeDir         func() (string, error)
	mkdirAll            func(path string, perm os.FileMode) error
	launchBrowser       func(profileDir, loginURL string) error
	waitForConfirmation func() error
}

func newTokenSaver() *tokenSaverImpl {
	return &tokenSaverImpl{
		tokenManagerFactory: func(credsPath, baseURL string, opts ...webauthn.TokenManagerOption) (tokenSaver, error) {
			return webauthn.NewTokenManager(credsPath, baseURL, opts...)
		},
		browserAuthFactory: func(baseURL string) browserAuthenticator {
			return &browserAuthAdapter{BrowserAuth: webauthn.NewBrowserAuth(baseURL)}
		},
		userHomeDir:         os.UserHomeDir,
		mkdirAll:            os.MkdirAll,
		launchBrowser:       launchChromeForManualLogin,
		waitForConfirmation: waitForBrowserConfirmation,
	}
}

type browserAuthAdapter struct {
	*webauthn.BrowserAuth
	authenticateFunc   func() (*webauthn.AuthTokens, error)
	extractTokensFunc  func() (*webauthn.AuthTokens, error)
	withHeadlessFunc   func(headless bool) *webauthn.BrowserAuth
	withProfileDirFunc func(profileDir string) *webauthn.BrowserAuth
}

func (a *browserAuthAdapter) browserAuthOrError() (*webauthn.BrowserAuth, error) {
	if a == nil || a.BrowserAuth == nil {
		return nil, errors.New("browser auth is not configured")
	}

	return a.BrowserAuth, nil
}

func (a *browserAuthAdapter) Authenticate() (*webauthn.AuthTokens, error) {
	if a.authenticateFunc != nil {
		return a.authenticateFunc()
	}

	browserAuth, err := a.browserAuthOrError()
	if err != nil {
		return nil, err
	}

	return browserAuth.Authenticate()
}

func (a *browserAuthAdapter) ExtractTokensFromProfile() (*webauthn.AuthTokens, error) {
	if a.extractTokensFunc != nil {
		return a.extractTokensFunc()
	}

	browserAuth, err := a.browserAuthOrError()
	if err != nil {
		return nil, err
	}

	return browserAuth.ExtractTokensFromProfile()
}

func (a *browserAuthAdapter) WithHeadless(headless bool) browserAuthenticator {
	if a.withHeadlessFunc != nil {
		return &browserAuthAdapter{BrowserAuth: a.withHeadlessFunc(headless)}
	}

	browserAuth, err := a.browserAuthOrError()
	if err != nil {
		return &browserAuthAdapter{}
	}

	return &browserAuthAdapter{BrowserAuth: browserAuth.WithHeadless(headless)}
}

func (a *browserAuthAdapter) WithProfileDir(profileDir string) browserAuthenticator {
	if a.withProfileDirFunc != nil {
		return &browserAuthAdapter{BrowserAuth: a.withProfileDirFunc(profileDir)}
	}

	browserAuth, err := a.browserAuthOrError()
	if err != nil {
		return &browserAuthAdapter{}
	}

	return &browserAuthAdapter{BrowserAuth: browserAuth.WithProfileDir(profileDir)}
}

// prepareDirectories creates the private config and browser profile folders.
func (ts *tokenSaverImpl) prepareDirectories() (string, string, string, error) {
	homeDir, err := ts.userHomeDir()
	if err != nil {
		return "", "", "", fmt.Errorf("failed to get home directory: %w", err)
	}
	configDir := filepath.Join(homeDir, defaultConfigDirName)
	if err := ts.mkdirAll(configDir, 0o700); err != nil {
		return "", "", "", fmt.Errorf("failed to create config directory: %w", err)
	}
	tokensPath := filepath.Join(configDir, authTokensFileName)
	profileDir := os.Getenv("CHROME_PROFILE_DIR")
	if profileDir == "" {
		profileDir = filepath.Join(configDir, "chrome-profile")
	}
	if err := ts.mkdirAll(profileDir, 0o700); err != nil {
		return "", "", "", fmt.Errorf("failed to create chrome profile directory: %w", err)
	}
	return configDir, tokensPath, profileDir, nil
}

func (ts *tokenSaverImpl) run() error {
	fmt.Println("🌐 Autenticação Hourglass + Salvamento de Tokens")
	fmt.Println("⏱️  Você tem 5 minutos para completar a autenticação")
	fmt.Println("👁️  A janela do Chrome será visível")
	fmt.Println()

	configDir, tokensPath, profileDir, err := ts.prepareDirectories()
	if err != nil {
		return err
	}

	fmt.Printf("💾 Tokens serão salvos em: %s\n", tokensPath)
	fmt.Printf("🌐 Perfil do Chrome será salvo em: %s\n", profileDir)
	fmt.Println()
	fmt.Println("🧭 Fluxo de bootstrap manual: Chrome normal -> login humano -> extração automática dos cookies do perfil")
	fmt.Println()

	tm, err := ts.tokenManagerFactory(
		filepath.Join(configDir, "webauthn-credentials.json"),
		appBaseURL,
		webauthn.WithTokensPath(tokensPath),
		webauthn.WithOnTokenRenewed(onTokenRenewed),
	)
	if err != nil {
		return fmt.Errorf("failed to create TokenManager: %w", err)
	}

	loginURL := appBaseURL + loginPagePath
	if ts.launchBrowser != nil {
		if err := ts.launchBrowser(profileDir, loginURL); err != nil {
			return fmt.Errorf("failed to launch manual browser: %w", err)
		}
	}

	if ts.waitForConfirmation != nil {
		if err := ts.waitForConfirmation(); err != nil {
			return fmt.Errorf("manual browser confirmation failed: %w", err)
		}
	}

	browserAuth := ts.browserAuthFactory(appBaseURL).WithHeadless(false).WithProfileDir(profileDir)

	tokens, err := browserAuth.ExtractTokensFromProfile()
	if err != nil {
		return fmt.Errorf("profile token extraction failed: %w", err)
	}

	err = tm.SaveTokens(tokens)
	if err != nil {
		return fmt.Errorf("failed to save tokens: %w", err)
	}

	return nil
}

func launchChromeForManualLogin(profileDir, loginURL string) error {
	chromePath := os.Getenv("CHROME_BIN")
	if chromePath == "" {
		chromePath = os.Getenv("CHROME_PATH")
	}
	if chromePath == "" {
		for _, candidate := range []string{"/usr/bin/google-chrome", "/usr/bin/google-chrome-stable", "/usr/bin/chromium-browser", "/usr/bin/chromium"} {
			if _, err := chromeStatFn(candidate); err == nil {
				chromePath = candidate
				break
			}
		}
	}
	if chromePath == "" {
		return fmt.Errorf("chrome/chromium not found: set CHROME_BIN environment variable or install Chrome")
	}

	if err := prepareChromeProfileFn(profileDir); err != nil {
		return err
	}

	cmd := execCommandFn(chromePath,
		fmt.Sprintf("--user-data-dir=%s", profileDir),
		"--new-window",
		"--no-first-run",
		"--no-default-browser-check",
		loginURL,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return err
	}

	return nil
}

func waitForBrowserConfirmation() error {
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("🔐 O Chrome normal foi aberto com o perfil persistente.")
	fmt.Println("👉 Faça o login manualmente no Google/Hourglass, confirme que chegou no app e feche a janela do Chrome.")
	fmt.Print("⏎ Depois disso, pressione Enter para extrair os tokens do perfil salvo... ")
	_, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("failed to read confirmation input: %w", err)
	}
	return nil
}

func printSuccess(tokensPath string, tokens *webauthn.AuthTokens) {
	fmt.Println()
	fmt.Println("✅✅✅ SUCESSO! ✅✅✅")
	fmt.Println()
	fmt.Println("🔑 Tokens extraídos e salvos:")
	fmt.Printf("   HGLogin:  %s...%s\n", tokens.HGLogin[:4], tokens.HGLogin[len(tokens.HGLogin)-4:])
	fmt.Printf("   XSRF:     %s...%s\n", tokens.XSRFToken[:4], tokens.XSRFToken[len(tokens.XSRFToken)-4:])
	fmt.Printf("   Expira:   %s\n", tokens.ExpiresAt.Format(tokenDateTimeFormat))
	fmt.Println()
	fmt.Printf("💾 Arquivo: %s\n", tokensPath)
	fmt.Println()
	fmt.Println("🚀 Agora você pode copiar esse arquivo para a VPS para uso imediato:")
	fmt.Printf("   scp %s user@vps:%s\n", tokensPath, vpsTokensPathTemplate)
	fmt.Println()
	fmt.Println("💡 Para renovação automática na VPS, execute também: make setup-auth")
}

func main() {
	if err := newTokenSaverForMain().run(); err != nil {
		logFatal(err)
	}

	homeDir, _ := userHomeDirForMain()
	configDir := filepath.Join(homeDir, defaultConfigDirName)
	tokensPath := filepath.Join(configDir, authTokensFileName)

	tm, err := newTokenLoader(configDir, tokensPath)
	if err != nil {
		logFatal("Failed to load tokens for display:", err)
	}

	tokens, err := tm.LoadTokens()
	if err != nil {
		logFatal("Failed to load saved tokens:", err)
	}

	printSuccessFn(tokensPath, tokens)
}
