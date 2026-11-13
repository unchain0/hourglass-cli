package hourglass

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewAPIAnalyzer(t *testing.T) {
	client := NewClient()
	analyzer := NewAPIAnalyzer(client)

	assert.NotNil(t, analyzer)
	assert.NotNil(t, analyzer.client)
	assert.NotNil(t, analyzer.userCache)
	assert.Equal(t, 48092, analyzer.congregationID)
}

func TestAPIAnalyzer_SetCongregationID(t *testing.T) {
	client := NewClient()
	analyzer := NewAPIAnalyzer(client)

	analyzer.SetCongregationID(12345)
	assert.Equal(t, 12345, analyzer.congregationID)
}

func TestAPIAnalyzer_AnalyzeSection_UnknownSection(t *testing.T) {
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(UsersResponse{Users: []User{}})
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	analyzer := NewAPIAnalyzer(client)

	result, err := analyzer.AnalyzeSection("UnknownSection")
	require.NoError(t, err)
	assert.Error(t, result.Error)
	assert.Contains(t, result.Error.Error(), "unknown section")
}

func TestAPIAnalyzer_LoadUsersError(t *testing.T) {
	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	analyzer := NewAPIAnalyzer(client)

	result, err := analyzer.AnalyzeSection("Mechanical Parts")
	require.NoError(t, err)
	assert.Error(t, result.Error)
	assert.Contains(t, result.Error.Error(), "failed to load users")
}

func TestAPIAnalyzer_GetNotificationsError(t *testing.T) {
	users := []User{{ID: 1, Firstname: "Test"}}

	startRange := time.Now().Format("2006-01-02")
	endRange := time.Now().AddDate(0, 0, 730).Format("2006-01-02")
	expectedPath := fmt.Sprintf("/scheduling/notifications/%s_%s/ava", startRange, endRange)

	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fsreport/users":
			json.NewEncoder(w).Encode(UsersResponse{Users: users})
		case expectedPath:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	analyzer := NewAPIAnalyzer(client)

	result, err := analyzer.AnalyzeSection("Mechanical Parts")
	require.NoError(t, err)
	assert.Error(t, result.Error)
}

func TestAPIAnalyzer_AnalyzeAllSections(t *testing.T) {
	users := []User{{ID: 1, Firstname: "Test", Descriptor: "Test User"}}

	startRange := time.Now().Format("2006-01-02")
	endRange := time.Now().AddDate(0, 0, 730).Format("2006-01-02")
	pathAva := fmt.Sprintf("/scheduling/notifications/%s_%s/ava", startRange, endRange)
	pathFm := fmt.Sprintf("/scheduling/notifications/%s_%s/fm", startRange, endRange)
	pathPubwit := fmt.Sprintf("/scheduling/notifications/%s_%s/pubwit", startRange, endRange)
	pathMm := fmt.Sprintf("/scheduling/notifications/%s_%s/mm", startRange, endRange)
	meetingPath := fmt.Sprintf("/scheduling/mm/meeting/%s_%s", startRange, endRange)

	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fsreport/users":
			json.NewEncoder(w).Encode(UsersResponse{Users: users})
		case pathAva, pathFm, pathPubwit, pathMm:
			json.NewEncoder(w).Encode([]Notification{})
		case meetingPath:
			json.NewEncoder(w).Encode([]Meeting{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	analyzer := NewAPIAnalyzer(client)

	results, err := analyzer.AnalyzeAllSections()
	require.NoError(t, err)
	assert.Len(t, results, 4)
	assert.Equal(t, "Mechanical Parts", results[0].Section)
	assert.Equal(t, "Field Ministry", results[1].Section)
	assert.Equal(t, "Public Witnessing", results[2].Section)
	assert.Equal(t, "Midweek Meeting", results[3].Section)
}

func TestAPIAnalyzer_GetUserName(t *testing.T) {
	users := []User{
		{ID: 1, Firstname: "Pessoa", Lastname: "Exemplo A", Descriptor: "Pessoa Exemplo A"},
		{ID: 2, Firstname: "Maria", Lastname: "Santos"}, // No descriptor
	}

	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(UsersResponse{Users: users})
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	analyzer := NewAPIAnalyzer(client)

	// Load users
	analyzer.loadUsers()

	// Test getUserName
	name1 := analyzer.getUserName(1)
	assert.Equal(t, "Pessoa Exemplo A", name1)

	name2 := analyzer.getUserName(2)
	assert.Equal(t, "Maria Santos", name2)

	name3 := analyzer.getUserName(999) // Unknown user
	assert.Equal(t, "User 999", name3)
}

func TestAPIAnalyzer_UserCacheReuse(t *testing.T) {
	callCount := 0
	users := []User{{ID: 1, Firstname: "Test"}}

	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fsreport/users" {
			callCount++
			json.NewEncoder(w).Encode(UsersResponse{Users: users})
		} else {
			json.NewEncoder(w).Encode([]Notification{})
		}
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	analyzer := NewAPIAnalyzer(client)

	// First call should load users
	analyzer.AnalyzeSection("Mechanical Parts")
	assert.Equal(t, 1, callCount)

	// Second call should reuse cache
	analyzer.AnalyzeSection("Mechanical Parts")
	assert.Equal(t, 1, callCount) // Should not increase
}

func TestAPIAnalyzer_EmptyResponses(t *testing.T) {
	users := []User{{ID: 1, Firstname: "Test"}}

	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fsreport/users":
			json.NewEncoder(w).Encode(UsersResponse{Users: users})
		default:
			json.NewEncoder(w).Encode([]Notification{})
		}
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	analyzer := NewAPIAnalyzer(client)

	result, err := analyzer.AnalyzeSection("Mechanical Parts")
	require.NoError(t, err)
	assert.Equal(t, 0, result.Total)
	assert.Empty(t, result.Rejections)
}

func TestAPIAnalyzer_SectionAliases(t *testing.T) {
	users := []User{{ID: 1, Firstname: "Test"}}

	startRange := time.Now().Format("2006-01-02")
	endRange := time.Now().AddDate(0, 0, 730).Format("2006-01-02")
	pathAva := fmt.Sprintf("/scheduling/notifications/%s_%s/ava", startRange, endRange)
	pathFm := fmt.Sprintf("/scheduling/notifications/%s_%s/fm", startRange, endRange)
	pathPubwit := fmt.Sprintf("/scheduling/notifications/%s_%s/pubwit", startRange, endRange)

	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fsreport/users":
			json.NewEncoder(w).Encode(UsersResponse{Users: users})
		case pathAva, pathFm, pathPubwit:
			json.NewEncoder(w).Encode([]Notification{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	analyzer := NewAPIAnalyzer(client)

	// Test section aliases
	sections := []string{"avattendant", "fsMeeting", "publicWitnessing"}
	for _, section := range sections {
		result, err := analyzer.AnalyzeSection(section)
		require.NoError(t, err)
		require.NoError(t, result.Error)
	}
}

func TestAPIAnalyzer_PortugueseSectionNames(t *testing.T) {
	users := []User{{ID: 1, Firstname: "Test"}}

	startRange := time.Now().Format("2006-01-02")
	endRange := time.Now().AddDate(0, 0, 730).Format("2006-01-02")
	pathAva := fmt.Sprintf("/scheduling/notifications/%s_%s/ava", startRange, endRange)
	pathFm := fmt.Sprintf("/scheduling/notifications/%s_%s/fm", startRange, endRange)
	pathPubwit := fmt.Sprintf("/scheduling/notifications/%s_%s/pubwit", startRange, endRange)
	pathMm := fmt.Sprintf("/scheduling/notifications/%s_%s/mm", startRange, endRange)
	pathMeetings := fmt.Sprintf("/scheduling/mm/meeting/%s_%s", startRange, endRange)

	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/fsreport/users":
			json.NewEncoder(w).Encode(UsersResponse{Users: users})
		case r.URL.Path == pathAva, r.URL.Path == pathFm, r.URL.Path == pathPubwit, r.URL.Path == pathMm:
			json.NewEncoder(w).Encode([]Notification{})
		case strings.HasPrefix(r.URL.Path, pathMeetings):
			json.NewEncoder(w).Encode([]Meeting{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	analyzer := NewAPIAnalyzer(client)

	sections := []string{"Partes Mecânicas", "Campo", "Testemunho Público", "Reunião Meio de Semana"}
	for _, section := range sections {
		result, err := analyzer.AnalyzeSection(section)
		require.NoError(t, err)
		require.NoError(t, result.Error, "Section %s should be recognized", section)
	}
}

func TestAPIAnalyzer_MultipleDeclined(t *testing.T) {
	users := []User{
		{ID: 1, Firstname: "Pessoa", Lastname: "Exemplo A", Descriptor: "Pessoa Exemplo A"},
		{ID: 2, Firstname: "Maria", Lastname: "Santos", Descriptor: "Maria Santos"},
	}

	notifications := []Notification{
		// Multiple declined notifications
		{
			ID:             1,
			CongregationID: 48092,
			Date:           "2026-03-01",
			Type:           "pubwit",
			Status:         "declined",
			Assignee:       1,
			Part:           100,
		},
		{
			ID:             2,
			CongregationID: 48092,
			Date:           "2026-03-02",
			Type:           "pubwit",
			Status:         "declined",
			Assignee:       2,
			Part:           101,
		},
		{
			ID:             3,
			CongregationID: 48092,
			Date:           "2026-03-03",
			Type:           "pubwit",
			Status:         "pending", // Not declined
			Assignee:       1,
			Part:           102,
		},
	}

	startRange := time.Now().Format("2006-01-02")
	endRange := time.Now().AddDate(0, 0, 730).Format("2006-01-02")
	expectedPath := fmt.Sprintf("/scheduling/notifications/%s_%s/pubwit", startRange, endRange)

	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fsreport/users":
			json.NewEncoder(w).Encode(UsersResponse{Users: users})
		case expectedPath:
			json.NewEncoder(w).Encode(notifications)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	analyzer := NewAPIAnalyzer(client)

	result, err := analyzer.AnalyzeSection("Public Witnessing")
	require.NoError(t, err)
	require.NoError(t, result.Error)
	assert.Equal(t, 2, result.Total)
	assert.Len(t, result.Rejections, 2)
	assert.Equal(t, "Pessoa Exemplo A", result.Rejections[0].Who)
	assert.Equal(t, "Maria Santos", result.Rejections[1].Who)
	assert.Equal(t, "2026-03-01", result.Rejections[0].When)
	assert.Equal(t, "2026-03-02", result.Rejections[1].When)
}

func TestAPIAnalyzer_AnalyzeMidweekMeetings(t *testing.T) {
	users := []User{{ID: 1, Firstname: "Pessoa", Lastname: "Exemplo A", Descriptor: "Pessoa Exemplo A"}}

	// Create meetings with parts
	meetings := []Meeting{{
		TGW: []MeetingPart{{ID: 100, Title: "Bible Reading"}},
		FM:  []MeetingPart{{ID: 101, Title: "Presentation"}},
		LAC: []MeetingPart{{ID: 102, Title: "Discussion"}},
	}}

	notifications := []Notification{
		{
			ID:             1,
			CongregationID: 48092,
			Date:           "2026-03-01",
			Type:           "mm",
			Status:         "declined",
			Assignee:       1,
			Part:           100,
			Flag:           10,
		},
		{
			ID:             2,
			CongregationID: 48092,
			Date:           "2026-03-02",
			Type:           "mm",
			Status:         "declined",
			Assignee:       1,
			Part:           101,
			Flag:           20,
		},
		{
			ID:             3,
			CongregationID: 48092,
			Date:           "2026-03-03",
			Type:           "mm",
			Status:         "pending",
			Assignee:       1,
			Part:           102,
			Flag:           30,
		},
	}

	startRange := time.Now().Format("2006-01-02")
	endRange := time.Now().AddDate(0, 0, 730).Format("2006-01-02")
	expectedPath := fmt.Sprintf("/scheduling/notifications/%s_%s/mm", startRange, endRange)
	meetingPath := fmt.Sprintf("/scheduling/mm/meeting/%s_%s", startRange, endRange)

	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fsreport/users":
			json.NewEncoder(w).Encode(UsersResponse{Users: users})
		case expectedPath:
			json.NewEncoder(w).Encode(notifications)
		case meetingPath:
			json.NewEncoder(w).Encode(meetings)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	analyzer := NewAPIAnalyzer(client)

	rejections, err := analyzer.analyzeMidweekMeetings()
	require.NoError(t, err)
	assert.Len(t, rejections, 2)
	assert.Equal(t, "Bible Reading", rejections[0].What)
	assert.Equal(t, "Presentation", rejections[1].What)
}

func TestAPIAnalyzer_AnalyzeMidweekMeetings_GetNotificationsError(t *testing.T) {
	users := []User{{ID: 1, Firstname: "Test"}}

	startRange := time.Now().Format("2006-01-02")
	endRange := time.Now().AddDate(0, 0, 730).Format("2006-01-02")
	expectedPath := fmt.Sprintf("/scheduling/notifications/%s_%s/mm", startRange, endRange)

	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fsreport/users":
			json.NewEncoder(w).Encode(UsersResponse{Users: users})
		case expectedPath:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	analyzer := NewAPIAnalyzer(client)

	_, err := analyzer.analyzeMidweekMeetings()
	assert.Error(t, err)
}

func TestAPIAnalyzer_AnalyzeMidweekMeetings_GetMeetingsError(t *testing.T) {
	users := []User{{ID: 1, Firstname: "Test"}}

	notifications := []Notification{{
		ID:             1,
		CongregationID: 48092,
		Date:           "2026-03-01",
		Type:           "mm",
		Status:         "declined",
		Assignee:       1,
		Part:           100,
		Flag:           10,
	}}

	startRange := time.Now().Format("2006-01-02")
	endRange := time.Now().AddDate(0, 0, 730).Format("2006-01-02")
	expectedPath := fmt.Sprintf("/scheduling/notifications/%s_%s/mm", startRange, endRange)
	meetingPath := fmt.Sprintf("/scheduling/mm/meeting/%s_%s", startRange, endRange)

	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fsreport/users":
			json.NewEncoder(w).Encode(UsersResponse{Users: users})
		case expectedPath:
			json.NewEncoder(w).Encode(notifications)
		case meetingPath:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	analyzer := NewAPIAnalyzer(client)

	_, err := analyzer.analyzeMidweekMeetings()
	assert.Error(t, err)
}

func TestAPIAnalyzer_AnalyzeMidweekMeetings_FallbackFlagName(t *testing.T) {
	users := []User{{ID: 1, Firstname: "Pessoa", Lastname: "Exemplo A", Descriptor: "Pessoa Exemplo A"}}

	// Create meetings without the part ID
	meetings := []Meeting{{TGW: []MeetingPart{{ID: 999, Title: "Some Other Part"}}}}

	notifications := []Notification{{
		ID:             1,
		CongregationID: 48092,
		Date:           "2026-03-01",
		Type:           "mm",
		Status:         "declined",
		Assignee:       1,
		Part:           100, // Not in meetings
		Flag:           30,
	}}

	startRange := time.Now().Format("2006-01-02")
	endRange := time.Now().AddDate(0, 0, 730).Format("2006-01-02")
	expectedPath := fmt.Sprintf("/scheduling/notifications/%s_%s/mm", startRange, endRange)
	meetingPath := fmt.Sprintf("/scheduling/mm/meeting/%s_%s", startRange, endRange)

	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fsreport/users":
			json.NewEncoder(w).Encode(UsersResponse{Users: users})
		case expectedPath:
			json.NewEncoder(w).Encode(notifications)
		case meetingPath:
			json.NewEncoder(w).Encode(meetings)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	analyzer := NewAPIAnalyzer(client)

	rejections, err := analyzer.analyzeMidweekMeetings()
	require.NoError(t, err)
	assert.Len(t, rejections, 1)
	assert.Equal(t, "Student", rejections[0].What)
}

func TestAPIAnalyzer_Deduplication(t *testing.T) {
	users := []User{
		{ID: 1, Firstname: "Pessoa", Lastname: "Exemplo B", Descriptor: "Pessoa Exemplo B"},
	}

	notifications := []Notification{
		{
			ID:             1,
			CongregationID: 48092,
			Date:           "2026-03-13",
			Type:           "pubwit",
			Status:         "declined",
			Assignee:       1,
			Part:           100,
		},
		{
			ID:             2,
			CongregationID: 48092,
			Date:           "2026-03-13",
			Type:           "pubwit",
			Status:         "declined",
			Assignee:       1,
			Part:           100,
		},
	}

	startRange := time.Now().Format("2006-01-02")
	endRange := time.Now().AddDate(0, 0, 730).Format("2006-01-02")
	expectedPath := fmt.Sprintf("/scheduling/notifications/%s_%s/pubwit", startRange, endRange)

	server := newHTTPTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fsreport/users":
			json.NewEncoder(w).Encode(UsersResponse{Users: users})
		case expectedPath:
			json.NewEncoder(w).Encode(notifications)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClient()
	client.baseURL = server.URL
	analyzer := NewAPIAnalyzer(client)

	result, err := analyzer.AnalyzeSection("Public Witnessing")
	require.NoError(t, err)
	require.NoError(t, result.Error)
	assert.Equal(t, 1, result.Total, "should deduplicate identical rejections")
	assert.Len(t, result.Rejections, 1)
	assert.Equal(t, "Pessoa Exemplo B", result.Rejections[0].Who)
	assert.Equal(t, "Public Witnessing", result.Rejections[0].What)
	assert.Equal(t, "2026-03-13", result.Rejections[0].When)
}

func TestAPIAnalyzer_SetDaysToLookAhead(t *testing.T) {
	client := NewClient()
	analyzer := NewAPIAnalyzer(client)

	analyzer.SetDaysToLookAhead(365)
	assert.Equal(t, 365, analyzer.daysToLookAhead)

	analyzer.SetDaysToLookAhead(730)
	assert.Equal(t, 730, analyzer.daysToLookAhead)
}

func TestGetFriendlyTypeName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"ava", "Audio/Video & Indicators"},
		{"video", "Video"},
		{"console", "Console"},
		{"mics", "Microphone"},
		{"attendant", "Attendant"},
		{"pubwit", "Public Witnessing"},
		{"fm", "Field Ministry Meeting"},
		{"unknown", "unknown"},
		{"", ""},
		{"custom", "custom"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := getFriendlyTypeName(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetMidweekFlagName(t *testing.T) {
	tests := []struct {
		flag     int
		expected string
	}{
		{10, "Bible Reading"},
		{11, "Bible Reading"},
		{20, "Speaker/Chairman"},
		{30, "Student"},
		{40, "Assistant"},
		{50, "Special Assignment"},
		{60, "Other Assignment"},
		{0, "Assignment (flag 0)"},
		{99, "Assignment (flag 99)"},
		{-1, "Assignment (flag -1)"},
		{100, "Assignment (flag 100)"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("flag_%d", tt.flag), func(t *testing.T) {
			result := getMidweekFlagName(tt.flag)
			assert.Equal(t, tt.expected, result)
		})
	}
}
