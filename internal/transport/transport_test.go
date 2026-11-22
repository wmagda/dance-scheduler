package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHubRecordAndParse(t *testing.T) {
	var recorded []string
	h := NewHub("manual", "", func(_ context.Context, ch Channel, to, body string) error {
		recorded = append(recorded, string(ch)+":"+to+":"+body)
		return nil
	})

	ctx := context.Background()
	if err := h.SendOTP(ctx, "instructor@example.com", "123456"); err != nil {
		t.Fatalf("SendOTP: %v", err)
	}
	if err := h.PostToGroup(ctx, "Resolved: Ana teaches Dec 3"); err != nil {
		t.Fatalf("PostToGroup: %v", err)
	}
	if len(recorded) != 2 {
		t.Fatalf("expected 2 recorded messages, got %d", len(recorded))
	}

	cmd, err := h.ParseCommand("/token 2026-12-03")
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	if cmd.Type != CmdToken || cmd.DateArg != "2026-12-03" {
		t.Fatalf("unexpected command: %+v", cmd)
	}
}

func TestEmailTransportResend(t *testing.T) {
	var gotAuth string
	var gotPayload map[string]any

	mockResend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emails" || r.Method != http.MethodPost {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotPayload)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"email_123"}`))
	}))
	defer mockResend.Close()

	h := NewHubWithEmail("email", "", EmailConfig{
		APIKey:  "re_test_key",
		From:    "Scrumpy's <noreply@scrumpys.codeciech.com>",
		BaseURL: mockResend.URL,
	}, nil)

	ctx := context.Background()
	if err := h.SendOTP(ctx, "dancer@example.com", "654321"); err != nil {
		t.Fatalf("SendOTP via Resend: %v", err)
	}
	if gotAuth != "Bearer re_test_key" {
		t.Fatalf("expected Bearer auth header, got %q", gotAuth)
	}
	subj, _ := gotPayload["subject"].(string)
	if !strings.Contains(subj, "654321") {
		t.Fatalf("expected OTP code in subject, got %q", subj)
	}
}
