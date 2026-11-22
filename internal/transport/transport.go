// Package transport defines the messaging abstraction layer so the core
// scheduler never couples directly to Email (Resend), WhatsApp, Telegram, or manual copy-paste.
package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Channel identifies the logical destination of an outbound message.
type Channel string

const (
	ChannelGroup   Channel = "group"
	ChannelDM      Channel = "dm"
	ChannelOTP     Channel = "otp"
	ChannelFBEvent Channel = "fb_event"
)

// Recipient holds transport-agnostic addressing for a teacher.
type Recipient struct {
	TeacherID int64  `json:"teacher_id"`
	Name      string `json:"name"`
	Email     string `json:"email,omitempty"`
	Phone     string `json:"phone,omitempty"`
	Handle    string `json:"handle,omitempty"`
}

// EmailConfig configures transactional email delivery via the Resend HTTP API.
type EmailConfig struct {
	APIKey  string // Resend API key (re_...)
	From    string // e.g. "Scrumpy's Latin Dancing <noreply@scrumpys.codeciech.com>"
	BaseURL string // Optional override for testing (defaults to https://api.resend.com)
}

// CommandType is a parsed inbound bot slash-command.
type CommandType string

const (
	CmdClaim  CommandType = "claim"
	CmdToken  CommandType = "token"
	CmdLead   CommandType = "lead"
	CmdFollow CommandType = "follow"
	CmdStatus CommandType = "status"
	CmdHelp   CommandType = "help"
)

// Command represents a parsed inbound command from WhatsApp or Telegram.
type Command struct {
	Type    CommandType
	DateArg string // YYYY-MM-DD or raw date string
	Raw     string
}

// Transport is the messaging interface consumed by the scheduler and web server.
// Swapping Email (Resend), WhatsApp, Telegram, or Manual Digest is a configuration change.
type Transport interface {
	Name() string
	SendDM(ctx context.Context, to Recipient, text string) error
	PostToGroup(ctx context.Context, text string) error
	SendOTP(ctx context.Context, target string, code string) error
	ParseCommand(raw string) (*Command, error)
}

// RecordFunc persists an outbound message to the database outbox so the admin
// UI always has a copy-paste digest and audit trail even if a bot is offline.
type RecordFunc func(ctx context.Context, ch Channel, recipient string, body string) error

// Hub implements Transport by recording outbound messages into the persistent
// outbox and delivering via Resend Email (when mode == "email") or an optional
// chat bot webhook (WhatsApp / Telegram).
type Hub struct {
	mode       string
	webhookURL string
	emailCfg   EmailConfig
	record     RecordFunc
	client     *http.Client
}

// NewHub constructs a transport Hub without email credentials.
// mode may be "manual", "email", "whatsapp", or "telegram".
func NewHub(mode, webhookURL string, record RecordFunc) *Hub {
	return NewHubWithEmail(mode, webhookURL, EmailConfig{}, record)
}

// NewHubWithEmail constructs a transport Hub with optional Resend EmailConfig.
func NewHubWithEmail(mode, webhookURL string, emailCfg EmailConfig, record RecordFunc) *Hub {
	if mode == "" {
		mode = "manual"
	}
	if strings.TrimSpace(emailCfg.From) == "" {
		emailCfg.From = "Scrumpy's Latin Dancing <noreply@scrumpys.codeciech.com>"
	}
	if strings.TrimSpace(emailCfg.BaseURL) == "" {
		emailCfg.BaseURL = "https://api.resend.com"
	}
	return &Hub{
		mode:       mode,
		webhookURL: strings.TrimSpace(webhookURL),
		emailCfg:   emailCfg,
		record:     record,
		client:     &http.Client{Timeout: 10 * time.Second},
	}
}

func (h *Hub) Name() string { return h.mode }

func (h *Hub) SendDM(ctx context.Context, to Recipient, text string) error {
	addr := to.Email
	if addr == "" {
		addr = to.Phone
	}
	if h.mode == "telegram" && to.Handle != "" {
		addr = to.Handle
	}
	label := to.Name
	if addr != "" {
		label = fmt.Sprintf("%s (%s)", to.Name, addr)
	}
	if h.record != nil {
		if err := h.record(ctx, ChannelDM, label, text); err != nil {
			return err
		}
	}
	if h.mode == "email" && strings.TrimSpace(to.Email) != "" && strings.TrimSpace(h.emailCfg.APIKey) != "" {
		// Best-effort DM email: outbox already captured the notification, so don't
		// abort state transitions if an instructor's mailbox bounces.
		_ = h.sendResendEmail(ctx, to.Email, "Thursday Latin Dancing @ Scrumpy's — Schedule Update", text)
		return nil
	}
	return h.forwardWebhook(ctx, map[string]any{
		"channel":   "dm",
		"transport": h.mode,
		"recipient": to,
		"text":      text,
	})
}

func (h *Hub) PostToGroup(ctx context.Context, text string) error {
	if h.record != nil {
		if err := h.record(ctx, ChannelGroup, "Instructors Group", text); err != nil {
			return err
		}
	}
	return h.forwardWebhook(ctx, map[string]any{
		"channel":   "group",
		"transport": h.mode,
		"text":      text,
	})
}

func (h *Hub) SendOTP(ctx context.Context, target string, code string) error {
	msg := fmt.Sprintf("Your Thursday Latin Dancing at Scrumpy's login code is: %s\n\nThis code expires in 10 minutes.", code)
	if h.record != nil {
		// Record audit entry without storing live plaintext OTP codes in the UI outbox.
		if err := h.record(ctx, ChannelOTP, target, "6-digit login code dispatched (expires in 10m)"); err != nil {
			return err
		}
	}
	if h.mode == "email" {
		if strings.TrimSpace(h.emailCfg.APIKey) == "" {
			return fmt.Errorf("email transport selected but RESEND_API_KEY is not configured")
		}
		subj := fmt.Sprintf("Your Scrumpy's Scheduler code: %s", code)
		return h.sendResendEmail(ctx, target, subj, msg)
	}
	return h.forwardWebhook(ctx, map[string]any{
		"channel":   "otp",
		"transport": h.mode,
		"phone":     target,
		"email":     target,
		"code":      code,
		"text":      msg,
	})
}

func (h *Hub) sendResendEmail(ctx context.Context, toEmail, subject, textBody string) error {
	payload := map[string]any{
		"from":    h.emailCfg.From,
		"to":      []string{strings.TrimSpace(toEmail)},
		"subject": subject,
		"text":    textBody,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(h.emailCfg.BaseURL, "/") + "/emails"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(h.emailCfg.APIKey))
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("resend request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("resend error (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func (h *Hub) forwardWebhook(ctx context.Context, payload map[string]any) error {
	if h.webhookURL == "" || h.mode == "manual" || h.mode == "email" {
		return nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.webhookURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		// Degrade gracefully: outbox already captured the message.
		return nil
	}
	defer resp.Body.Close()
	return nil
}

// ParseCommand parses inbound chat commands like "/claim 2026-12-03" or "/status".
func (h *Hub) ParseCommand(raw string) (*Command, error) {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "/") {
		return nil, fmt.Errorf("not a command")
	}
	parts := strings.Fields(s)
	verb := strings.ToLower(strings.TrimPrefix(parts[0], "/"))
	arg := ""
	if len(parts) > 1 {
		arg = strings.Join(parts[1:], " ")
	}
	switch verb {
	case "claim", "bid":
		if arg == "" {
			return nil, fmt.Errorf("usage: /claim YYYY-MM-DD")
		}
		return &Command{Type: CmdClaim, DateArg: arg, Raw: raw}, nil
	case "token", "veto", "reserve":
		if arg == "" {
			return nil, fmt.Errorf("usage: /token YYYY-MM-DD")
		}
		return &Command{Type: CmdToken, DateArg: arg, Raw: raw}, nil
	case "lead":
		if arg == "" {
			return nil, fmt.Errorf("usage: /lead YYYY-MM-DD")
		}
		return &Command{Type: CmdLead, DateArg: arg, Raw: raw}, nil
	case "follow":
		if arg == "" {
			return nil, fmt.Errorf("usage: /follow YYYY-MM-DD")
		}
		return &Command{Type: CmdFollow, DateArg: arg, Raw: raw}, nil
	case "status", "me":
		return &Command{Type: CmdStatus, Raw: raw}, nil
	case "help":
		return &Command{Type: CmdHelp, Raw: raw}, nil
	default:
		return nil, fmt.Errorf("unknown command %q (try /help)", parts[0])
	}
}
