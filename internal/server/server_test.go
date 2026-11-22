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

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "gate.db")
	st, err := store.Open(dbPath, "http://localhost:8095", "manual", "")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now().UTC()
	if err := st.EnsureUpcomingThursdays(context.Background(), now); err != nil {
		t.Fatalf("EnsureUpcomingThursdays: %v", err)
	}
	if _, err := st.CreateTeacher(context.Background(), "Wojtek", "+19705550100", "@wojtek", true, now.Year()); err != nil {
		t.Fatalf("CreateTeacher: %v", err)
	}
	srv := httptest.NewServer(New(st))
	t.Cleanup(srv.Close)
	return srv
}

func loginSession(t *testing.T, srvURL string) *http.Cookie {
	t.Helper()
	reqBody, _ := json.Marshal(map[string]string{"phone": "9705550100"})
	resp, err := http.Post(srvURL+"/api/auth/request-otp", "application/json", bytes.NewReader(reqBody))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("request-otp: status=%v err=%v", resp.StatusCode, err)
	}
	var otpResp struct {
		ManualModeCode string `json:"manual_mode_code"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&otpResp)
	resp.Body.Close()
	if len(otpResp.ManualModeCode) != 6 {
		t.Fatalf("expected manual code for loopback request, got %q", otpResp.ManualModeCode)
	}
	verBody, _ := json.Marshal(map[string]string{"phone": "9705550100", "code": otpResp.ManualModeCode})
	resp2, err := http.Post(srvURL+"/api/auth/verify-otp", "application/json", bytes.NewReader(verBody))
	if err != nil || resp2.StatusCode != 200 {
		t.Fatalf("verify-otp: status=%v err=%v", resp2.StatusCode, err)
	}
	defer resp2.Body.Close()
	cookies := resp2.Cookies()
	if len(cookies) == 0 {
		t.Fatalf("expected session cookie")
	}
	return cookies[0]
}

// The whole API is members-only: unauthenticated callers may only reach the
// two login endpoints and logout. Static assets stay public for the login page.
func TestSessionGateBlocksAnonymous(t *testing.T) {
	srv := newTestServer(t)

	resp, err := http.Get(srv.URL + "/api/state")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous /api/state: want 401, got %d", resp.StatusCode)
	}

	for _, path := range []string{"/api/bids", "/api/admin/resolve"} {
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader([]byte("{}")))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("anonymous %s: want 401, got %d", path, resp.StatusCode)
		}
	}

	// Static assets must remain public so the login page renders.
	respStatic, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("static: %v", err)
	}
	respStatic.Body.Close()
	if respStatic.StatusCode != 200 {
		t.Fatalf("anonymous /: want 200, got %d", respStatic.StatusCode)
	}

	// Authenticated requests still pass the gate.
	cookie := loginSession(t, srv.URL)
	req, _ := http.NewRequest("GET", srv.URL+"/api/state", nil)
	req.AddCookie(cookie)
	respAuth, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("authed state: %v", err)
	}
	respAuth.Body.Close()
	if respAuth.StatusCode != 200 {
		t.Fatalf("authed /api/state: want 200, got %d", respAuth.StatusCode)
	}
}

// Manual-mode OTP codes must never reach a non-local requester.
func TestManualModeCodeSuppressedForRemote(t *testing.T) {
	// httptest connections are loopback, so simulate a remote client by
	// rewriting RemoteAddr on a raw handler request.
	req := httptest.NewRequest("POST", "/api/auth/request-otp",
		bytes.NewReader([]byte(`{"phone":"9705550100"}`)))
	req.RemoteAddr = "203.0.113.7:51000"
	rec := httptest.NewRecorder()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"), "http://localhost:8095", "manual", "")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	_ = st.EnsureUpcomingThursdays(context.Background(), time.Now().UTC())
	if _, err := st.CreateTeacher(context.Background(), "W", "+19705550100", "", true, time.Now().UTC().Year()); err != nil {
		t.Fatalf("CreateTeacher: %v", err)
	}
	New(st).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("request-otp: want 200, got %d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if _, leaked := body["manual_mode_code"]; leaked {
		t.Fatalf("manual_mode_code leaked to non-local request: %v", body)
	}
}

// The bot webhook only accepts loopback callers (the WhatsApp bridge runs on
// this host); remote clients are rejected before reaching the handler.
func TestBotWebhookLoopbackOnly(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "bot.db"), "http://localhost:8095", "manual", "")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	s := New(st)

	req := httptest.NewRequest("POST", "/api/bot/inbound", bytes.NewReader([]byte(`{"phone":"+19705550100","text":"/status"}`)))
	req.RemoteAddr = "203.0.113.7:51000"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote bot webhook: want 403, got %d", rec.Code)
	}

	reqLocal := httptest.NewRequest("POST", "/api/bot/inbound", bytes.NewReader([]byte(`{"phone":"+19705550100","text":"/status"}`)))
	reqLocal.RemoteAddr = "127.0.0.1:51001"
	recLocal := httptest.NewRecorder()
	s.ServeHTTP(recLocal, reqLocal)
	if recLocal.Code == http.StatusForbidden {
		t.Fatalf("loopback bot webhook should not be 403, got %d", recLocal.Code)
	}
}

// request-otp is rate-limited per phone to stop code spam and enumeration.
func TestRequestOTPRateLimited(t *testing.T) {
	srv := newTestServer(t)
	for i := 0; i < otpRequestMaxPerPhone; i++ {
		reqBody, _ := json.Marshal(map[string]string{"phone": "9705550100"})
		resp, err := http.Post(srv.URL+"/api/auth/request-otp", "application/json", bytes.NewReader(reqBody))
		if err != nil {
			t.Fatalf("request-otp: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("request %d: want 200, got %d", i+1, resp.StatusCode)
		}
	}
	reqBody, _ := json.Marshal(map[string]string{"phone": "9705550100"})
	resp, err := http.Post(srv.URL+"/api/auth/request-otp", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("request-otp: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("rate limit: want 429, got %d", resp.StatusCode)
	}
}

// verify-otp guesses are capped per phone.
func TestVerifyOTPAttemptCap(t *testing.T) {
	srv := newTestServer(t)
	for i := 0; i < otpVerifyMaxPerPhone; i++ {
		reqBody, _ := json.Marshal(map[string]string{"phone": "9705550100", "code": "000000"})
		resp, err := http.Post(srv.URL+"/api/auth/verify-otp", "application/json", bytes.NewReader(reqBody))
		if err != nil {
			t.Fatalf("verify-otp: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: want 401, got %d", i+1, resp.StatusCode)
		}
	}
	reqBody, _ := json.Marshal(map[string]string{"phone": "9705550100", "code": "000000"})
	resp, err := http.Post(srv.URL+"/api/auth/verify-otp", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("verify-otp: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("attempt cap: want 429, got %d", resp.StatusCode)
	}
}

// Invite-gated self-signup: a non-roster email can register only with an
// active invite token, is added to the roster only AFTER verifying the 6-digit
// code, and is blocked once the invite link is revoked.
func TestInviteSignupFlow(t *testing.T) {
	srv := newTestServer(t)

	// 1. Unknown email without invite -> forbidden.
	body, _ := json.Marshal(map[string]string{"email": "newteacher@example.com", "name": "New Person"})
	resp, err := http.Post(srv.URL+"/api/auth/request-otp", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request-otp: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no-invite signup: want 403, got %d", resp.StatusCode)
	}

	// 2. Login as admin and fetch the invite token + expiration metadata.
	cookie := loginSession(t, srv.URL)
	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/invite", nil)
	req.AddCookie(cookie)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get invite: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("get invite: want 200, got %d", resp2.StatusCode)
	}
	var inv struct {
		InviteToken string `json:"invite_token"`
		ExpiresAt   string `json:"expires_at"`
		Active      bool   `json:"active"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&inv)
	if !inv.Active || len(inv.InviteToken) < 16 || inv.ExpiresAt == "" {
		t.Fatalf("expected active expiring invite token, got %+v", inv)
	}

	// 3. Bad token still rejected.
	bodyBad, _ := json.Marshal(map[string]string{"email": "newteacher@example.com", "name": "New Person", "invite": "deadbeefdeadbeef"})
	resp3, err := http.Post(srv.URL+"/api/auth/request-otp", "application/json", bytes.NewReader(bodyBad))
	if err != nil {
		t.Fatalf("request-otp bad invite: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusForbidden {
		t.Fatalf("bad-invite signup: want 403, got %d", resp3.StatusCode)
	}

	// 4. Valid token + name + email -> code issued, verify creates roster account & session.
	bodyOK, _ := json.Marshal(map[string]string{"email": "newteacher@example.com", "name": "New Person", "invite": inv.InviteToken})
	resp4, err := http.Post(srv.URL+"/api/auth/request-otp", "application/json", bytes.NewReader(bodyOK))
	if err != nil {
		t.Fatalf("request-otp signup: %v", err)
	}
	var otpResp struct {
		ManualModeCode string `json:"manual_mode_code"`
	}
	_ = json.NewDecoder(resp4.Body).Decode(&otpResp)
	resp4.Body.Close()
	if resp4.StatusCode != 200 || len(otpResp.ManualModeCode) != 6 {
		t.Fatalf("invite signup: want 200+code, got %d code=%q", resp4.StatusCode, otpResp.ManualModeCode)
	}
	verBody, _ := json.Marshal(map[string]string{"email": "newteacher@example.com", "code": otpResp.ManualModeCode})
	resp5, err := http.Post(srv.URL+"/api/auth/verify-otp", "application/json", bytes.NewReader(verBody))
	if err != nil || resp5.StatusCode != 200 {
		t.Fatalf("verify-otp after signup: status=%v err=%v", resp5.StatusCode, err)
	}
	resp5.Body.Close()

	// 5. Revoke invite link -> subsequent signups rejected with 403.
	reqRev, _ := http.NewRequest("POST", srv.URL+"/api/admin/invite/revoke", bytes.NewReader([]byte("{}")))
	reqRev.AddCookie(cookie)
	respRev, err := http.DefaultClient.Do(reqRev)
	if err != nil || respRev.StatusCode != 200 {
		t.Fatalf("revoke invite: status=%v err=%v", respRev.StatusCode, err)
	}
	respRev.Body.Close()

	bodyAfterRev, _ := json.Marshal(map[string]string{"email": "another@example.com", "name": "Another", "invite": inv.InviteToken})
	resp6, err := http.Post(srv.URL+"/api/auth/request-otp", "application/json", bytes.NewReader(bodyAfterRev))
	if err != nil {
		t.Fatalf("request-otp after revoke: %v", err)
	}
	resp6.Body.Close()
	if resp6.StatusCode != http.StatusForbidden {
		t.Fatalf("signup after revoke: want 403, got %d", resp6.StatusCode)
	}
}

// cloudflared forwards public internet traffic over 127.0.0.1 with CF-Connecting-IP;
// ensure such requests never pass loopback/LAN checks.
func TestCloudflareTunnelLoopbackProtection(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "cf.db"), "https://scrumpys.codeciech.com", "manual", "")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	_ = st.EnsureUpcomingThursdays(context.Background(), time.Now().UTC())
	if _, err := st.SaveTeacher(context.Background(), "Admin", "admin@codeciech.com", "+19705550100", "", true, 2026); err != nil {
		t.Fatalf("SaveTeacher: %v", err)
	}
	s := New(st)

	// 1. /api/bot/inbound via cloudflared (127.0.0.1 + CF-Connecting-IP) must 403.
	reqBot := httptest.NewRequest("POST", "/api/bot/inbound", bytes.NewReader([]byte(`{"phone":"+19705550100","text":"/status"}`)))
	reqBot.RemoteAddr = "127.0.0.1:52000"
	reqBot.Header.Set("CF-Connecting-IP", "198.51.100.77")
	recBot := httptest.NewRecorder()
	s.ServeHTTP(recBot, reqBot)
	if recBot.Code != http.StatusForbidden {
		t.Fatalf("cloudflared bot webhook: want 403, got %d", recBot.Code)
	}

	// 2. Manual-mode OTP via cloudflared must NOT leak manual_mode_code.
	reqOTP := httptest.NewRequest("POST", "/api/auth/request-otp", bytes.NewReader([]byte(`{"email":"admin@codeciech.com"}`)))
	reqOTP.RemoteAddr = "127.0.0.1:52001"
	reqOTP.Header.Set("CF-Connecting-IP", "198.51.100.77")
	recOTP := httptest.NewRecorder()
	s.ServeHTTP(recOTP, reqOTP)
	if recOTP.Code != http.StatusOK {
		t.Fatalf("cloudflared request-otp: want 200, got %d (%s)", recOTP.Code, recOTP.Body.String())
	}
	var otpBody map[string]any
	_ = json.Unmarshal(recOTP.Body.Bytes(), &otpBody)
	if _, leaked := otpBody["manual_mode_code"]; leaked {
		t.Fatalf("manual_mode_code leaked over cloudflared loopback proxy: %v", otpBody)
	}
}
