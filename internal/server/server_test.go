package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/wojtek/dance-scheduler/internal/store"
)

func TestHTTPFlowEndToEnd(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "http.db")
	st, err := store.Open(dbPath, "http://localhost:8095", "manual", "")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	now := time.Now().UTC()
	if err := st.EnsureUpcomingThursdays(ctx, now); err != nil {
		t.Fatalf("EnsureUpcomingThursdays: %v", err)
	}
	if _, err := st.CreateTeacher(ctx, "Wojtek", "+19705550100", "@wojtek", true, now.Year()); err != nil {
		t.Fatalf("CreateTeacher: %v", err)
	}

	srv := httptest.NewServer(New(st))
	defer srv.Close()

	// 1. Request OTP in manual mode
	reqBody, _ := json.Marshal(map[string]string{"phone": "9705550100"})
	resp, err := http.Post(srv.URL+"/api/auth/request-otp", "application/json", bytes.NewReader(reqBody))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("request-otp failed: status=%v err=%v", resp.StatusCode, err)
	}
	var otpResp struct {
		ManualModeCode string `json:"manual_mode_code"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&otpResp)
	resp.Body.Close()
	if len(otpResp.ManualModeCode) != 6 {
		t.Fatalf("expected 6-digit manual_mode_code, got %q", otpResp.ManualModeCode)
	}

	// 2. Verify OTP and capture session cookie
	verBody, _ := json.Marshal(map[string]string{"phone": "9705550100", "code": otpResp.ManualModeCode})
	resp2, err := http.Post(srv.URL+"/api/auth/verify-otp", "application/json", bytes.NewReader(verBody))
	if err != nil || resp2.StatusCode != 200 {
		t.Fatalf("verify-otp failed: status=%v err=%v", resp2.StatusCode, err)
	}
	cookies := resp2.Cookies()
	resp2.Body.Close()
	if len(cookies) == 0 {
		t.Fatalf("expected session cookie")
	}

	// 3. Fetch /api/state authenticated
	reqState, _ := http.NewRequest("GET", srv.URL+"/api/state", nil)
	reqState.AddCookie(cookies[0])
	resp3, err := http.DefaultClient.Do(reqState)
	if err != nil || resp3.StatusCode != 200 {
		t.Fatalf("state failed: %v", err)
	}
	var state struct {
		Viewer *store.Teacher    `json:"viewer"`
		Events []store.EventView `json:"events"`
	}
	_ = json.NewDecoder(resp3.Body).Decode(&state)
	resp3.Body.Close()
	if state.Viewer == nil || !state.Viewer.IsAdmin || len(state.Events) == 0 {
		t.Fatalf("unexpected state: viewer=%+v events=%d", state.Viewer, len(state.Events))
	}
}
