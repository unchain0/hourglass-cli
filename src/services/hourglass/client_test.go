package hourglass

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hourglass-rejections-rpa/src/integrations/auth/webauthn"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubTokenManager struct {
	current         *webauthn.AuthTokens
	renewed         *webauthn.AuthTokens
	ensureCalls     int
	forceRenewCalls int
	forceRenewalErr error
}

func TestMain(m *testing.M) {
	originalNewHTTPRequest := newHTTPRequest
	code := m.Run()
	newHTTPRequest = originalNewHTTPRequest
	os.Exit(code)
}

func (s *stubTokenManager) Start(context.Context) error { return nil }

func (s *stubTokenManager) Stop() {}

func (s *stubTokenManager) PrimeTokens(tokens *webauthn.AuthTokens) {
	s.current = tokens
}

func (s *stubTokenManager) EnsureValidTokens() (*webauthn.AuthTokens, error) {
	s.ensureCalls++
	return s.current, nil
}

func (s *stubTokenManager) ForceRenewal() (*webauthn.AuthTokens, error) {
	s.forceRenewCalls++
	if s.forceRenewalErr != nil {
		return nil, s.forceRenewalErr
	}
	s.current = s.renewed
	return s.current, nil
}

func TestNewClient(t *testing.T) {
	client := NewClient()
	assert.NotNil(t, client)
	assert.NotNil(t, client.httpClient)
	assert.Equal(t, defaultBaseURL, client.baseURL)
}

func TestClient_SetBaseURL_UsesSiteRoot(t *testing.T) {
	client := NewClient()
	client.SetBaseURL("https://hourglass.example.com")
	assert.Equal(t, "https://hourglass.example.com/api/v0.2", client.baseURL)
}

func TestClient_SetBaseURL_KeepsAPIPath(t *testing.T) {
	client := NewClient()
	client.SetBaseURL("https://hourglass.example.com/api/v0.2")
	assert.Equal(t, "https://hourglass.example.com/api/v0.2", client.baseURL)
}

func TestClient_SetBaseURL_UsesDefaultWhenEmpty(t *testing.T) {
	client := NewClient()
	client.SetBaseURL("")
	assert.Equal(t, defaultBaseURL, client.baseURL)
}

func TestClient_EnableWebAuthn_PrimesConfiguredSession(t *testing.T) {
	client := NewClient()
	client.SetHGLogin("bootstrap-hg-login")
	client.SetXSRFToken("bootstrap-xsrf-token")

	err := client.EnableWebAuthn(filepath.Join(t.TempDir(), "credentials.json"), nil)
	require.NoError(t, err)

	manager, ok := client.tokenManager.(*webauthn.TokenManager)
	require.True(t, ok)
	tokens := manager.GetTokens()
	require.NotNil(t, tokens)
	assert.Equal(t, "bootstrap-hg-login", tokens.HGLogin)
	assert.Equal(t, "bootstrap-xsrf-token", tokens.XSRFToken)
	assert.True(t, tokens.IsExpired())
}

func TestClient_SetBaseURL_KeepsInvalidURLUntouched(t *testing.T) {
	client := NewClient()
	client.SetBaseURL("://bad-url")
	assert.Equal(t, "://bad-url", client.baseURL)
}

func TestNormalizeAPIBaseURL_ParseErrorReturnsOriginal(t *testing.T) {
	invalidURL := "http://[::1"
	assert.Equal(t, invalidURL, normalizeAPIBaseURL(invalidURL))
}

func TestClient_SetXSRFToken(t *testing.T) {
	client := NewClient()
	client.SetXSRFToken("test-token-123")
	assert.Equal(t, "test-token-123", client.xsrfToken)
}

func TestClient_GetUsers(t *testing.T) {
	expectedUsers := []User{
		{
			ID:         31944,
			Firstname:  "João",
			Lastname:   "Exemplo A",
			Descriptor: "Pessoa Exemplo A",
			Appt:       "Elder",
		},
		{
			ID:         31945,
			Firstname:  "Maria",
			Lastname:   "Santos",
			Descriptor: "Maria Santos",
			Appt:       "MS",
		},
	}

	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/fsreport/users", r.URL.Path)
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Accept"))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(UsersResponse{Users: expectedUsers})
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL

	users, err := client.GetUsers()
	require.NoError(t, err)
	assert.Len(t, users, 2)
	assert.Equal(t, "João", users[0].Firstname)
	assert.Equal(t, "Maria", users[1].Firstname)
}

func TestClient_GetUsers_Error(t *testing.T) {
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error": "unauthorized"}`))
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL

	users, err := client.GetUsers()
	assert.Error(t, err)
	assert.Nil(t, users)
	assert.Contains(t, err.Error(), "unexpected status code: 401")
}

func TestClient_GetAVAttendants(t *testing.T) {
	expected := []AVAttendant{
		{
			ID:        57427,
			Type:      "attendant",
			Assignee:  nil,
			Slot:      1,
			Date:      "2026-03-01",
			Extra:     false,
			Confirmed: false,
			Published: true,
		},
		{
			ID:        57428,
			Type:      "video",
			Assignee:  new(31944),
			Slot:      1,
			Date:      "2026-03-01",
			Extra:     false,
			Confirmed: true,
			Published: true,
		},
	}

	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/scheduling/av_attendant/2026-03-01_2026-03-07", r.URL.Path)
		assert.Equal(t, http.MethodGet, r.Method)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(expected)
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL

	attendants, err := client.GetAVAttendants("2026-03-01", "2026-03-07")
	require.NoError(t, err)
	assert.Len(t, attendants, 2)
	assert.Equal(t, "attendant", attendants[0].Type)
	assert.Nil(t, attendants[0].Assignee)
	assert.Equal(t, 31944, *attendants[1].Assignee)
}

func TestClient_GetMeetings(t *testing.T) {
	expected := []Meeting{
		{
			Date:   "2026-03-02",
			LGroup: 48092,
			TGW: []MeetingPart{
				{ID: 123, Title: "Joias Espirituais", Info: "", Time: "10 min", Type: "dfg"},
			},
			FM: []MeetingPart{
				{ID: 124, Title: "Iniciando conversas", Info: "", Time: "3 min", Type: "initcall"},
			},
			LAC: []MeetingPart{
				{ID: 125, Title: "Estudo bíblico", Info: "", Time: "30 min", Type: "cbs"},
			},
		},
	}

	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/scheduling/mm/meeting/2026-03-01_2026-03-07", r.URL.Path)
		assert.Equal(t, "lgroup=48092&no_subs=true", r.URL.RawQuery)
		assert.Equal(t, http.MethodGet, r.Method)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(expected)
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL

	meetings, err := client.GetMeetings("2026-03-01", "2026-03-07", 48092)
	require.NoError(t, err)
	assert.Len(t, meetings, 1)
	assert.Equal(t, "2026-03-02", meetings[0].Date)
	assert.Equal(t, 1, len(meetings[0].TGW))
}

func TestClient_GetMeetings_Error(t *testing.T) {
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL

	meetings, err := client.GetMeetings("2026-03-01", "2026-03-07", 48092)
	assert.Error(t, err)
	assert.Nil(t, meetings)
}

func TestClient_WithXSRFToken(t *testing.T) {
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Hourglass-XSRF-Token")
		assert.Equal(t, "KizSvAQQ4B6lCkHNGagusFtX5nRjlbVZ", token)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(UsersResponse{Users: []User{}})
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	client.SetXSRFToken("KizSvAQQ4B6lCkHNGagusFtX5nRjlbVZ")

	_, err := client.GetUsers()
	require.NoError(t, err)
}

func TestClient_Timeout(t *testing.T) {
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	client.httpClient.Timeout = 50 * time.Millisecond

	_, err := client.GetUsers()
	assert.Error(t, err)
}

func TestClient_GetUsers_DecodeError(t *testing.T) {
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`invalid json`))
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL

	users, err := client.GetUsers()
	assert.Error(t, err)
	assert.Nil(t, users)
	assert.Contains(t, err.Error(), "failed to decode response")
}

func TestClient_GetAVAttendants_DecodeError(t *testing.T) {
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{invalid`))
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL

	attendants, err := client.GetAVAttendants("2026-03-01", "2026-03-07")
	assert.Error(t, err)
	assert.Nil(t, attendants)
}

func TestClient_GetAVAttendants_StatusError(t *testing.T) {
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error": "forbidden"}`))
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL

	attendants, err := client.GetAVAttendants("2026-03-01", "2026-03-07")
	assert.Error(t, err)
	assert.Nil(t, attendants)
	assert.Contains(t, err.Error(), "unexpected status code: 403")
}

func TestClient_GetMeetings_DecodeError(t *testing.T) {
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{`))
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL

	meetings, err := client.GetMeetings("2026-03-01", "2026-03-07", 48092)
	assert.Error(t, err)
	assert.Nil(t, meetings)
}

func TestModels(t *testing.T) {
	// Test User model
	user := User{
		ID:         1,
		Firstname:  "Test",
		Lastname:   "User",
		Descriptor: "Test User",
		Appt:       "Elder",
	}
	assert.Equal(t, 1, user.ID)
	assert.Equal(t, "Test", user.Firstname)

	// Test AVAttendant with nil Assignee
	attendant := AVAttendant{
		ID:        1,
		Type:      "video",
		Assignee:  nil,
		Slot:      1,
		Date:      "2026-03-01",
		Extra:     false,
		Confirmed: false,
		Published: true,
	}
	assert.Nil(t, attendant.Assignee)

	// Test AssignmentStatus constants
	assert.Equal(t, AssignmentStatus("Aceito"), StatusAceito)
	assert.Equal(t, AssignmentStatus("Recusado"), StatusRecusado)
}

func TestClient_InvalidURL(t *testing.T) {
	client := NewClient()
	client.baseURL = "://invalid-url"

	_, err := client.GetUsers()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create request")
}

func TestClient_GetAVAttendants_InvalidURL(t *testing.T) {
	client := NewClient()
	client.baseURL = "://invalid"

	_, err := client.GetAVAttendants("2026-03-01", "2026-03-07")
	assert.Error(t, err)
}

func TestClient_GetMeetings_InvalidURL(t *testing.T) {
	client := NewClient()
	client.baseURL = "://invalid"

	_, err := client.GetMeetings("2026-03-01", "2026-03-07", 48092)
	assert.Error(t, err)
}

func TestClient_EmptyResponse(t *testing.T) {
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"users": []}`))
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL

	users, err := client.GetUsers()
	require.NoError(t, err)
	assert.Empty(t, users)
}

func TestClient_GetUsers_NetworkError(t *testing.T) {
	client := NewClient()
	client.baseURL = "http://localhost:1"

	_, err := client.GetUsers()
	assert.Error(t, err)
}

func TestClient_GetAVAttendants_NetworkError(t *testing.T) {
	client := NewClient()
	client.baseURL = "http://localhost:1"

	_, err := client.GetAVAttendants("2026-03-01", "2026-03-07")
	assert.Error(t, err)
}

func TestClient_GetMeetings_NetworkError(t *testing.T) {
	client := NewClient()
	client.baseURL = "http://localhost:1"

	_, err := client.GetMeetings("2026-03-01", "2026-03-07", 48092)
	assert.Error(t, err)
}

func TestClient_GetNotifications(t *testing.T) {
	expected := []Notification{
		{
			ID:             1,
			CongregationID: 48092,
			Date:           "2026-03-01",
			Type:           "pubwit",
			Status:         "declined",
			Assignee:       123,
			Part:           100,
		},
		{
			ID:             2,
			CongregationID: 48092,
			Date:           "2026-03-02",
			Type:           "pubwit",
			Status:         "pending",
			Assignee:       456,
			Part:           101,
		},
	}

	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/scheduling/notifications/2026-03-01_2026-03-31/pubwit", r.URL.Path)
		assert.Equal(t, http.MethodGet, r.Method)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(expected)
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL

	notifications, err := client.GetNotifications("2026-03-01", "2026-03-31", "pubwit")
	require.NoError(t, err)
	assert.Len(t, notifications, 2)
	assert.Equal(t, "declined", notifications[0].Status)
	assert.Equal(t, "pending", notifications[1].Status)
}

func TestClient_GetNotifications_Error(t *testing.T) {
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL

	notifications, err := client.GetNotifications("2026-03-01", "2026-03-31", "pubwit")
	assert.Error(t, err)
	assert.Nil(t, notifications)
	assert.Contains(t, err.Error(), "unexpected status code: 500")
}

func TestClient_GetNotifications_DecodeError(t *testing.T) {
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{invalid`))
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL

	notifications, err := client.GetNotifications("2026-03-01", "2026-03-31", "pubwit")
	assert.Error(t, err)
	assert.Nil(t, notifications)
	assert.Contains(t, err.Error(), "failed to decode response")
}

func TestClient_GetNotifications_InvalidURL(t *testing.T) {
	client := NewClient()
	client.baseURL = "://invalid"

	_, err := client.GetNotifications("2026-03-01", "2026-03-31", "pubwit")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create request")
}

func TestClient_GetNotifications_NetworkError(t *testing.T) {
	client := NewClient()
	client.baseURL = "http://localhost:1"

	_, err := client.GetNotifications("2026-03-01", "2026-03-31", "pubwit")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to execute request")
}

func TestClient_SetHGLogin(t *testing.T) {
	client := NewClient()
	assert.Empty(t, client.hgLogin)

	client.SetHGLogin("test-hglogin-cookie")
	assert.Equal(t, "test-hglogin-cookie", client.hgLogin)
}

func TestClient_setCookies_WithHGLogin(t *testing.T) {
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("hglogin")
		assert.NoError(t, err)
		assert.Equal(t, "my-session-cookie", cookie.Value)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(UsersResponse{Users: []User{}})
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	client.SetHGLogin("my-session-cookie")

	_, err := client.GetUsers()
	require.NoError(t, err)
}

func TestClient_setCookies_Empty(t *testing.T) {
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := r.Cookie("hglogin")
		assert.Error(t, err, "hglogin cookie should not be present")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(UsersResponse{Users: []User{}})
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL

	_, err := client.GetUsers()
	require.NoError(t, err)
}

func TestClient_LoadTokensFromFile(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		tmpDir := t.TempDir()
		tokensFile := tmpDir + "/auth-tokens.json"
		tokens := map[string]any{
			"hg_login":   "test-hg-login",
			"xsrf_token": "test-xsrf-token",
			"expires_at": time.Now().Add(8 * time.Hour).Format(time.RFC3339),
		}
		data, _ := json.Marshal(tokens)
		err := os.WriteFile(tokensFile, data, 0600)
		require.NoError(t, err)

		client := NewClient()
		err = client.LoadTokensFromFile(tokensFile)

		require.NoError(t, err)
		assert.Equal(t, "test-hg-login", client.hgLogin)
		assert.Equal(t, "test-xsrf-token", client.xsrfToken)
	})

	t.Run("file not found", func(t *testing.T) {
		client := NewClient()
		err := client.LoadTokensFromFile("/nonexistent/path/tokens.json")

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to read tokens file")
	})

	t.Run("invalid json", func(t *testing.T) {
		tmpDir := t.TempDir()
		tokensFile := tmpDir + "/auth-tokens.json"
		err := os.WriteFile(tokensFile, []byte("invalid json"), 0600)
		require.NoError(t, err)

		client := NewClient()
		err = client.LoadTokensFromFile(tokensFile)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to parse tokens")
	})

	t.Run("expired tokens", func(t *testing.T) {
		tmpDir := t.TempDir()
		tokensFile := tmpDir + "/auth-tokens.json"
		tokens := map[string]any{
			"hg_login":   "test-hg-login",
			"xsrf_token": "test-xsrf-token",
			"expires_at": time.Now().Add(-1 * time.Hour).Format(time.RFC3339),
		}
		data, _ := json.Marshal(tokens)
		err := os.WriteFile(tokensFile, data, 0600)
		require.NoError(t, err)

		client := NewClient()
		err = client.LoadTokensFromFile(tokensFile)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "tokens have expired")
	})
}

func TestClient_UpdateTokensFromManager(t *testing.T) {
	client := NewClient()
	tokens := &webauthn.AuthTokens{
		HGLogin:   "new-hg-login",
		XSRFToken: "new-xsrf-token",
	}

	client.UpdateTokensFromManager(tokens)

	assert.Equal(t, "new-hg-login", client.hgLogin)
	assert.Equal(t, "new-xsrf-token", client.xsrfToken)
}

func TestClient_StartTokenManager_NilManager(t *testing.T) {
	client := NewClient()

	err := client.StartTokenManager(t.Context())

	assert.NoError(t, err)
}

func TestClient_StopTokenManager_NilManager(_ *testing.T) {
	client := NewClient()

	client.StopTokenManager()
}

func TestClient_EnsureAuth_WebAuthnEnabledWithoutTokenManager(t *testing.T) {
	client := NewClient()
	client.useWebAuthn = true

	err := client.ensureAuth()

	assert.NoError(t, err)
}

func TestNewClientWithWebAuthn_InvalidCredentialsPath(t *testing.T) {
	client, err := NewClientWithWebAuthn("/nonexistent/webauthn-credentials.json", nil)

	assert.Error(t, err)
	assert.Nil(t, client)
	assert.Contains(t, err.Error(), "failed to create token manager")
}

func TestClient_EnableWebAuthn_InvalidCredentialsPath(t *testing.T) {
	client := NewClient()

	err := client.EnableWebAuthn("/nonexistent/webauthn-credentials.json", nil)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create token manager")
}

func TestNewClientWithWebAuthn_Success(t *testing.T) {
	credentialsPath := t.TempDir() + "/webauthn-credentials.json"

	client, err := NewClientWithWebAuthn(credentialsPath, nil)

	require.NoError(t, err)
	require.NotNil(t, client)
	assert.True(t, client.useWebAuthn)
	assert.NotNil(t, client.tokenManager)
	assert.Equal(t, defaultBaseURL, client.baseURL)
}

func TestClient_EnableWebAuthn_Success(t *testing.T) {
	client := NewClient()
	credentialsPath := t.TempDir() + "/webauthn-credentials.json"

	err := client.EnableWebAuthn(credentialsPath, nil)

	require.NoError(t, err)
	assert.True(t, client.useWebAuthn)
	assert.NotNil(t, client.tokenManager)
}

func TestClient_EnableWebAuthn_CallbackUpdatesTokensOnStart(t *testing.T) {
	tmpDir := t.TempDir()
	credentialsPath := tmpDir + "/webauthn-credentials.json"
	tokensPath := tmpDir + "/auth-tokens.json"
	tokens := &webauthn.AuthTokens{
		HGLogin:   "callback-hg-login",
		XSRFToken: "callback-xsrf-token",
		ExpiresAt: time.Now().Add(2 * time.Hour),
	}
	data, err := json.Marshal(tokens)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(tokensPath, data, 0o600))

	client := NewClient()
	err = client.EnableWebAuthn(credentialsPath, nil)
	require.NoError(t, err)

	err = client.StartTokenManager(t.Context())
	require.NoError(t, err)

	assert.Equal(t, "callback-hg-login", client.hgLogin)
	assert.Equal(t, "callback-xsrf-token", client.xsrfToken)
}

func TestClient_SetWebAuthnTokensPath(t *testing.T) {
	client := NewClient()
	client.SetWebAuthnTokensPath("/tmp/custom-auth-tokens.json")
	assert.Equal(t, "/tmp/custom-auth-tokens.json", client.webAuthnTokensPath)
}

func TestClient_SetBrowserProfileDir(t *testing.T) {
	client := NewClient()
	client.SetBrowserProfileDir("/tmp/chrome-profile")
	assert.Equal(t, "/tmp/chrome-profile", client.browserProfileDir)
}

func TestClient_NewTokenManager_UsesBrowserProfileDir(t *testing.T) {
	client := NewClient()
	client.SetWebAuthnTokensPath(filepath.Join(t.TempDir(), "tokens.json"))
	client.SetBrowserProfileDir(filepath.Join(t.TempDir(), "chrome-profile"))
	manager, err := client.newTokenManager(filepath.Join(t.TempDir(), "credentials.json"), nil)
	require.NoError(t, err)
	assert.NotNil(t, manager)
}

func TestClient_ForceRenewAuth_Disabled(t *testing.T) {
	client := NewClient()
	err := client.forceRenewAuth()
	assert.EqualError(t, err, "automatic token renewal is not enabled")
}

func TestClient_ForceRenewAuth_Success(t *testing.T) {
	tm := &stubTokenManager{
		renewed: &webauthn.AuthTokens{HGLogin: "renewed-hg", XSRFToken: "renewed-xsrf", ExpiresAt: time.Now().Add(time.Hour)},
	}
	client := NewClient()
	client.useWebAuthn = true
	client.tokenManager = tm
	err := client.forceRenewAuth()
	require.NoError(t, err)
	assert.Equal(t, "renewed-hg", client.hgLogin)
	assert.Equal(t, "renewed-xsrf", client.xsrfToken)
}

type sequenceRoundTripper struct {
	responses []*http.Response
	errs      []error
	index     int
}

func (rt *sequenceRoundTripper) RoundTrip(_ *http.Request) (*http.Response, error) {
	idx := rt.index
	rt.index++
	if idx < len(rt.errs) && rt.errs[idx] != nil {
		return nil, rt.errs[idx]
	}
	if idx < len(rt.responses) {
		return rt.responses[idx], nil
	}
	return nil, errors.New("unexpected round trip")
}

func TestClient_DoAuthenticatedGet_RetryRequestExecutionError(t *testing.T) {
	tm := &stubTokenManager{
		current: &webauthn.AuthTokens{HGLogin: "old", XSRFToken: "old", ExpiresAt: time.Now().Add(time.Hour)},
		renewed: &webauthn.AuthTokens{HGLogin: "new", XSRFToken: "new", ExpiresAt: time.Now().Add(2 * time.Hour)},
	}
	client := NewClient()
	client.useWebAuthn = true
	client.tokenManager = tm
	client.httpClient = &http.Client{Transport: &sequenceRoundTripper{
		responses: []*http.Response{{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("expired"))}},
		errs:      []error{nil, errors.New("retry boom")},
	}}

	resp, err := client.doAuthenticatedGet("http://example.com")
	assert.Nil(t, resp)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to execute request after forced token renewal")
}

func TestClient_DoAuthenticatedGet_PassThroughUnauthorizedWithoutWebAuthn(t *testing.T) {
	client := NewClient()
	client.httpClient = &http.Client{Transport: &sequenceRoundTripper{
		responses: []*http.Response{{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("expired"))}},
	}}

	resp, err := client.doAuthenticatedGet("http://example.com")
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	_ = resp.Body.Close()
}

func TestClient_DoAuthenticatedGet_PassThroughUnauthorizedWithMissingTokenManager(t *testing.T) {
	client := NewClient()
	client.useWebAuthn = true
	client.httpClient = &http.Client{Transport: &sequenceRoundTripper{
		responses: []*http.Response{{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("expired"))}},
	}}

	resp, err := client.doAuthenticatedGet("http://example.com")
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	_ = resp.Body.Close()
}

func TestClient_DoAuthenticatedGet_RetryRequestCreationError(t *testing.T) {
	originalNewHTTPRequest := newHTTPRequest
	callCount := 0
	newHTTPRequest = func(method, url string, body io.Reader) (*http.Request, error) {
		callCount++
		if callCount == 2 {
			return nil, errors.New("retry request boom")
		}
		return originalNewHTTPRequest(method, url, body)
	}
	defer func() { newHTTPRequest = originalNewHTTPRequest }()

	tm := &stubTokenManager{
		current: &webauthn.AuthTokens{HGLogin: "old", XSRFToken: "old", ExpiresAt: time.Now().Add(time.Hour)},
		renewed: &webauthn.AuthTokens{HGLogin: "new", XSRFToken: "new", ExpiresAt: time.Now().Add(2 * time.Hour)},
	}
	client := NewClient()
	client.useWebAuthn = true
	client.tokenManager = tm
	client.httpClient = &http.Client{Transport: &sequenceRoundTripper{
		responses: []*http.Response{{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("expired"))}},
	}}

	resp, err := client.doAuthenticatedGet("http://example.com")
	assert.Nil(t, resp)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create retry request")
}

func TestClient_GetUsers_RetriesAfterUnauthorizedByForcingRenewal(t *testing.T) {
	tokensBefore := &webauthn.AuthTokens{
		HGLogin:   "old-cookie",
		XSRFToken: "old-xsrf",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	tokensAfter := &webauthn.AuthTokens{
		HGLogin:   "new-cookie",
		XSRFToken: "new-xsrf",
		ExpiresAt: time.Now().Add(2 * time.Hour),
	}
	tm := &stubTokenManager{current: tokensBefore, renewed: tokensAfter}

	requestCount := 0
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		cookie, err := r.Cookie("hglogin")
		require.NoError(t, err)

		switch requestCount {
		case 1:
			assert.Equal(t, "old-cookie", cookie.Value)
			assert.Equal(t, "old-xsrf", r.Header.Get("X-Hourglass-XSRF-Token"))
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"expired session"}`))
		case 2:
			assert.Equal(t, "new-cookie", cookie.Value)
			assert.Equal(t, "new-xsrf", r.Header.Get("X-Hourglass-XSRF-Token"))
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(UsersResponse{Users: []User{{ID: 1, Firstname: "Retry"}}})
		default:
			t.Fatalf("unexpected extra request %d", requestCount)
		}
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	client.useWebAuthn = true
	client.tokenManager = tm

	users, err := client.GetUsers()
	require.NoError(t, err)
	require.Len(t, users, 1)
	assert.Equal(t, 1, tm.forceRenewCalls)
	assert.Equal(t, 1, tm.ensureCalls)
	assert.Equal(t, 2, requestCount)
	assert.Equal(t, "new-cookie", client.hgLogin)
	assert.Equal(t, "new-xsrf", client.xsrfToken)
}

func TestClient_GetUsers_ReportsForcedRenewalFailure(t *testing.T) {
	tm := &stubTokenManager{
		current: &webauthn.AuthTokens{
			HGLogin:   "old-cookie",
			XSRFToken: "old-xsrf",
			ExpiresAt: time.Now().Add(time.Hour),
		},
		forceRenewalErr: errors.New("renewal boom"),
	}

	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"expired session"}`))
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	client.useWebAuthn = true
	client.tokenManager = tm

	users, err := client.GetUsers()
	assert.Nil(t, users)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forced renewal failed")
	assert.Contains(t, err.Error(), "renewal boom")
	assert.Equal(t, 1, tm.forceRenewCalls)
}

func TestClient_EnableWebAuthn_UsesCustomTokensPath(t *testing.T) {
	tmpDir := t.TempDir()
	credentialsPath := tmpDir + "/webauthn-credentials.json"
	tokensPath := tmpDir + "/custom-auth-tokens.json"
	tokens := &webauthn.AuthTokens{
		HGLogin:   "custom-hg-login",
		XSRFToken: "custom-xsrf-token",
		ExpiresAt: time.Now().Add(2 * time.Hour),
	}
	data, err := json.Marshal(tokens)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(tokensPath, data, 0o600))

	client := NewClient()
	client.SetWebAuthnTokensPath(tokensPath)
	require.NoError(t, client.EnableWebAuthn(credentialsPath, nil))
	require.NoError(t, client.StartTokenManager(t.Context()))

	assert.Equal(t, "custom-hg-login", client.hgLogin)
	assert.Equal(t, "custom-xsrf-token", client.xsrfToken)
}

func TestClient_EnableWebAuthn_ErrorCallbackInvoked(t *testing.T) {
	t.Setenv("CI", "true")
	t.Setenv("CHROME_BIN", filepath.Join(t.TempDir(), "missing-chrome"))

	client := NewClient()
	tmpDir := t.TempDir()
	credentialsPath := tmpDir + "/webauthn-credentials.json"

	var capturedErr error
	err := client.EnableWebAuthn(credentialsPath, func(err error, extras map[string]any) {
		capturedErr = err
		assert.Equal(t, "token_manager", extras["component"])
		assert.Equal(t, "token_renewal", extras["action"])
	})
	require.NoError(t, err)

	err = client.ensureAuth()
	require.Error(t, err)
	require.Error(t, capturedErr)
}

func TestNewClientWithWebAuthn_CallbackUpdatesTokensOnStart(t *testing.T) {
	tmpDir := t.TempDir()
	credentialsPath := tmpDir + "/webauthn-credentials.json"
	tokensPath := tmpDir + "/auth-tokens.json"
	tokens := &webauthn.AuthTokens{
		HGLogin:   "constructor-hg-login",
		XSRFToken: "constructor-xsrf-token",
		ExpiresAt: time.Now().Add(2 * time.Hour),
	}
	data, err := json.Marshal(tokens)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(tokensPath, data, 0o600))

	t.Setenv("WEBAUTHN_TOKENS_PATH", tokensPath)
	client, err := NewClientWithWebAuthn(credentialsPath, nil)
	require.NoError(t, err)

	err = client.StartTokenManager(t.Context())
	require.NoError(t, err)

	assert.Equal(t, "constructor-hg-login", client.hgLogin)
	assert.Equal(t, "constructor-xsrf-token", client.xsrfToken)
}

func TestClient_StartTokenManager_Success(t *testing.T) {
	client := NewClient()
	tm := newTokenManagerWithPersistedTokens(t, &webauthn.AuthTokens{
		HGLogin:   "persisted-hg-login",
		XSRFToken: "persisted-xsrf-token",
		ExpiresAt: time.Now().Add(2 * time.Hour),
	})
	client.tokenManager = tm

	err := client.StartTokenManager(t.Context())

	require.NoError(t, err)
	stored := tm.GetTokens()
	require.NotNil(t, stored)
	assert.Equal(t, "persisted-hg-login", stored.HGLogin)
}

func TestClient_StartTokenManager_ErrorFromManager(t *testing.T) {
	client := NewClient()
	tmpDir := t.TempDir()
	credentialsPath := tmpDir + "/webauthn-credentials.json"
	tokensDir := tmpDir + "/tokens-dir"
	require.NoError(t, os.Mkdir(tokensDir, 0o700))

	tm, err := webauthn.NewTokenManager(credentialsPath, defaultBaseURL,
		webauthn.WithBrowserAuth(nil),
		webauthn.WithTokensPath(tokensDir),
	)
	require.NoError(t, err)
	client.tokenManager = tm

	err = client.StartTokenManager(t.Context())

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to load persisted tokens")
}

func TestClient_StopTokenManager_WithManager(t *testing.T) {
	client := NewClient()
	tm := newTokenManagerWithPersistedTokens(t, nil)
	client.tokenManager = tm

	client.StopTokenManager()
}

func TestClient_EnsureAuth_ErrorWhenTokenRenewalFails(t *testing.T) {
	client := newWebAuthnClientWithoutPersistedTokens(t)

	err := client.ensureAuth()

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to ensure authentication")
}

func TestClient_EnsureAuth_UpdatesTokensFromManager(t *testing.T) {
	client := NewClient()
	tm := newTokenManagerWithPersistedTokens(t, &webauthn.AuthTokens{
		HGLogin:   "manager-hg-login",
		XSRFToken: "manager-xsrf-token",
		ExpiresAt: time.Now().Add(2 * time.Hour),
	})
	client.tokenManager = tm
	client.useWebAuthn = true

	require.NoError(t, client.StartTokenManager(t.Context()))

	err := client.ensureAuth()

	require.NoError(t, err)
	assert.Equal(t, "manager-hg-login", client.hgLogin)
	assert.Equal(t, "manager-xsrf-token", client.xsrfToken)
}

func TestClient_GetUsers_EnsureAuthError(t *testing.T) {
	client := newWebAuthnClientWithoutPersistedTokens(t)

	users, err := client.GetUsers()

	assert.Error(t, err)
	assert.Nil(t, users)
	assert.Contains(t, err.Error(), "failed to ensure authentication")
}

func TestClient_GetAVAttendants_EnsureAuthError(t *testing.T) {
	client := newWebAuthnClientWithoutPersistedTokens(t)

	attendants, err := client.GetAVAttendants("2026-03-01", "2026-03-07")

	assert.Error(t, err)
	assert.Nil(t, attendants)
	assert.Contains(t, err.Error(), "failed to ensure authentication")
}

func TestClient_GetMeetings_EnsureAuthError(t *testing.T) {
	client := newWebAuthnClientWithoutPersistedTokens(t)

	meetings, err := client.GetMeetings("2026-03-01", "2026-03-07", 48092)

	assert.Error(t, err)
	assert.Nil(t, meetings)
	assert.Contains(t, err.Error(), "failed to ensure authentication")
}

func TestClient_GetNotifications_EnsureAuthError(t *testing.T) {
	client := newWebAuthnClientWithoutPersistedTokens(t)

	notifications, err := client.GetNotifications("2026-03-01", "2026-03-31", "pubwit")

	assert.Error(t, err)
	assert.Nil(t, notifications)
	assert.Contains(t, err.Error(), "failed to ensure authentication")
}

func newWebAuthnClientWithoutPersistedTokens(t *testing.T) *Client {
	t.Helper()
	client := NewClient()
	tmpDir := t.TempDir()
	credentialsPath := tmpDir + "/webauthn-credentials.json"
	tokensPath := tmpDir + "/auth-tokens.json"

	tm, err := webauthn.NewTokenManager(credentialsPath, defaultBaseURL,
		webauthn.WithBrowserAuth(nil),
		webauthn.WithTokensPath(tokensPath),
	)
	require.NoError(t, err)

	client.tokenManager = tm
	client.useWebAuthn = true
	return client
}

func newTokenManagerWithPersistedTokens(t *testing.T, tokens *webauthn.AuthTokens) *webauthn.TokenManager {
	t.Helper()
	tmpDir := t.TempDir()
	credentialsPath := tmpDir + "/webauthn-credentials.json"
	tokensPath := tmpDir + "/auth-tokens.json"

	if tokens != nil {
		data, err := json.Marshal(tokens)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(tokensPath, data, 0o600))
	}

	tm, err := webauthn.NewTokenManager(credentialsPath, defaultBaseURL,
		webauthn.WithBrowserAuth(nil),
		webauthn.WithTokensPath(tokensPath),
	)
	require.NoError(t, err)

	return tm
}

func TestClient_GetWM(t *testing.T) {
	assignee := 42
	expected := []WMAssignment{{
		ID: 1, Role: "chairman", Assignee: &assignee, Status: "Aceito", Date: "2026-01-04",
	}}
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/scheduling/wm/2026-01-01_2026-01-31", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(expected)
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	rows, err := client.GetWM("2026-01-01", "2026-01-31")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "chairman", rows[0].Role)
	assert.Equal(t, "Aceito", rows[0].Status)
	assert.Equal(t, 42, *rows[0].Assignee)
}

func TestClient_GetWM_Error(t *testing.T) {
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client := NewClient()
	client.baseURL = server.URL
	rows, err := client.GetWM("2026-01-01", "2026-01-31")
	assert.Error(t, err)
	assert.Nil(t, rows)
}

func TestClient_GetPrivileges(t *testing.T) {
	assignee := 7
	expected := []PrivilegeAssignment{{
		ID: 3, Type: "reader", Assignee: &assignee, Status: "Esperando", Date: "2026-01-04",
	}}
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/privileges", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(expected)
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	rows, err := client.GetPrivileges()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "reader", rows[0].Type)
	assert.Equal(t, "Esperando", rows[0].Status)
}

func TestClient_GetPrivileges_Error(t *testing.T) {
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	client := NewClient()
	client.baseURL = server.URL
	rows, err := client.GetPrivileges()
	assert.Error(t, err)
	assert.Nil(t, rows)
}

func TestClient_GetCheckRules(t *testing.T) {
	expected := []CheckRule{{
		ID: 9, Rule: "no-conflict", Status: "complete", Part: 10, Assignment: "Talk",
	}}
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/check-rules", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(expected)
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	rows, err := client.GetCheckRules()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "no-conflict", rows[0].Rule)
}

func TestClient_GetCheckRules_Error(t *testing.T) {
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	client := NewClient()
	client.baseURL = server.URL
	rows, err := client.GetCheckRules()
	assert.Error(t, err)
	assert.Nil(t, rows)
}

func TestClient_GetNotifications_EmptyType(t *testing.T) {
	client := NewClient()
	rows, err := client.GetNotifications("2026-01-01", "2026-01-31", "")
	assert.Nil(t, rows)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "notification type is required")
}

func TestClient_GetNotificationsByTypes(t *testing.T) {
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/mm"):
			json.NewEncoder(w).Encode([]Notification{{ID: 1, Type: "mm", Status: "declined"}})
		case strings.HasSuffix(r.URL.Path, "/ava"):
			json.NewEncoder(w).Encode([]Notification{{ID: 2, Type: "ava", Status: "pending"}})
		default:
			json.NewEncoder(w).Encode([]Notification{})
		}
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	rows, err := client.GetNotificationsByTypes("2026-01-01", "2026-01-31", []string{"mm", "ava"})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "mm", rows[0].Type)
	assert.Equal(t, "ava", rows[1].Type)
}

func TestClient_GetNotificationsByTypes_DefaultTypesAndError(t *testing.T) {
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/fm") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]Notification{})
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	rows, err := client.GetNotificationsByTypes("2026-01-01", "2026-01-31", nil)
	assert.Nil(t, rows)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "notifications type fm")
}
