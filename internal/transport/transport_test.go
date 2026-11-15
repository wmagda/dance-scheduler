package transport

import (
	"context"
	"testing"
)

func TestHubRecordAndParse(t *testing.T) {
	var recorded []string
	h := NewHub("manual", "", func(_ context.Context, ch Channel, to, body string) error {
		recorded = append(recorded, string(ch)+":"+to+":"+body)
		return nil
	})

	ctx := context.Background()
	if err := h.SendOTP(ctx, "+19705550101", "123456"); err != nil {
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
