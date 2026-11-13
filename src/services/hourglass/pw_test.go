package hourglass

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetPWLocationsAndScheduleUseAuthenticatedEndpoints(t *testing.T) {
	var gotPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.RequestURI())
		switch {
		case strings.HasSuffix(r.URL.Path, "/scheduling/pw/locations"):
			_, _ = w.Write([]byte(`[{"id":49038,"name":"Local Exemplo 1","disabled":false,"notes":"Posto"}]`))
		case strings.HasSuffix(r.URL.Path, "/scheduling/pw/schedule/2026-12-01_2026-12-31"):
			_, _ = w.Write([]byte(`[{"id":47025722,"location_id":49038,"datetime":"2026-12-02T08:00:00Z","duration":"2.00","assignee_id":2672077,"notes":null}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient()
	client.SetBaseURL(server.URL)

	locations, err := client.GetPWLocations()
	if err != nil {
		t.Fatalf("GetPWLocations: %v", err)
	}
	if len(locations) != 1 || locations[0].ID != 49038 || locations[0].Name != "Local Exemplo 1" || locations[0].Disabled {
		t.Fatalf("unexpected locations: %+v", locations)
	}

	slots, err := client.GetPWSchedule("2026-12-01", "2026-12-31")
	if err != nil {
		t.Fatalf("GetPWSchedule: %v", err)
	}
	if len(slots) != 1 || slots[0].LocationID != 49038 || slots[0].Duration != "2.00" || slots[0].AssigneeID == nil || *slots[0].AssigneeID != 2672077 {
		t.Fatalf("unexpected slots: %+v", slots)
	}

	want := []string{
		"/scheduling/pw/locations?disabled=true",
		"/scheduling/pw/schedule/2026-12-01_2026-12-31",
	}
	for i, suffix := range want {
		if !strings.HasSuffix(gotPaths[i], suffix) {
			t.Fatalf("request %d = %q, want suffix %q", i, gotPaths[i], suffix)
		}
	}
}

func TestGetPWEndpointsReportNon200WithoutLeakingBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("private failure detail"))
	}))
	defer server.Close()

	client := NewClient()
	client.SetBaseURL(server.URL)

	if _, err := client.GetPWLocations(); err == nil {
		t.Fatal("expected error for non-200 response")
	}
}

func TestUpsertPWSlotPutsOneDraftPerLocation(t *testing.T) {
	var body map[string]any
	var query, method string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, query = r.Method, r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"id":77,"location_id":10,"datetime":"2026-12-03T08:00:00Z","duration":"2.00","assignee_id":5,"notes":""}`))
	}))
	defer server.Close()

	client := NewClient()
	client.SetBaseURL(server.URL)
	publisher := 5
	saved, err := client.UpsertPWSlot(PWSlotDraft{
		LocationID: 10,
		Datetime:   "2026-12-03T08:00:00Z",
		Duration:   "2.00",
		AssigneeID: &publisher,
	})
	if err != nil {
		t.Fatalf("UpsertPWSlot: %v", err)
	}
	if saved.ID != 77 || saved.LocationID != 10 {
		t.Fatalf("unexpected slot: %+v", saved)
	}
	if method != http.MethodPut || !strings.Contains(query, "locID=10") {
		t.Fatalf("unexpected request: %s %s", method, query)
	}
	if body["id"] != float64(0) || body["assignee_id"] != float64(5) {
		t.Fatalf("unexpected payload: %+v", body)
	}
}

func TestUpsertPWSlotReportsRefusedWrite(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid request payload"}`))
	}))
	defer server.Close()

	client := NewClient()
	client.SetBaseURL(server.URL)
	if _, err := client.UpsertPWSlot(PWSlotDraft{LocationID: 1}); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("unexpected error: %v", err)
	}
}
