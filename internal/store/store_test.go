package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestFairnessTokensPrivacyAndRecruitment(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath, "http://localhost:8095", "manual", "")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	baseNow := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	if err := st.EnsureUpcomingThursdays(ctx, baseNow); err != nil {
		t.Fatalf("EnsureUpcomingThursdays: %v", err)
	}

	// Create 3 teachers: Regular (frequent), Newbie (passive/infrequent), Admin
	reg, err := st.CreateTeacher(ctx, "Carlos Regular", "9705550101", "@carlos", false, 2026)
	if err != nil {
		t.Fatalf("CreateTeacher reg: %v", err)
	}
	newbie, err := st.CreateTeacher(ctx, "Sofia New", "9705550102", "@sofia", false, 2026)
	if err != nil {
		t.Fatalf("CreateTeacher newbie: %v", err)
	}
	admin, err := st.CreateTeacher(ctx, "Wojtek", "9705550100", "@wojtek", true, 2026)
	if err != nil {
		t.Fatalf("CreateTeacher admin: %v", err)
	}

	events, err := st.ListEventsForViewer(ctx, newbie.ID, false)
	if err != nil || len(events) < 4 {
		t.Fatalf("ListEventsForViewer: err=%v len=%d", err, len(events))
	}
	ev1 := events[0] // 2026-11-19
	ev2 := events[1] // 2026-11-26
	ev3 := events[2] // 2026-12-03
	ev4 := events[3] // 2026-12-10

	// 1. Carlos bids on ev1 alone and wins at resolution -> Carlos now has 1 confirmed night in window
	if err := st.PlaceBid(ctx, reg.ID, ev1.ID, "normal", baseNow); err != nil {
		t.Fatalf("PlaceBid ev1: %v", err)
	}
	resolved, err := st.RunResolver(ctx, baseNow, ev1.ID)
	if err != nil || resolved != 1 {
		t.Fatalf("RunResolver ev1: resolved=%d err=%v", resolved, err)
	}

	// 2. Contested date (ev2): Carlos bids first, Sofia bids 2 days later.
	// Privacy check before resolution: Sofia should NOT see Carlos's name on ev2!
	if err := st.PlaceBid(ctx, reg.ID, ev2.ID, "normal", baseNow); err != nil {
		t.Fatalf("Carlos bid ev2: %v", err)
	}
	viewForSofia, err := st.GetEventView(ctx, ev2.ID, newbie.ID, false)
	if err != nil {
		t.Fatalf("GetEventView for Sofia: %v", err)
	}
	if len(viewForSofia.AdminBids) != 0 || viewForSofia.HelpRequester != "" {
		t.Fatalf("expected bidder identity hidden from Sofia, got %+v", viewForSofia)
	}

	if err := st.PlaceBid(ctx, newbie.ID, ev2.ID, "normal", baseNow.Add(48*time.Hour)); err != nil {
		t.Fatalf("Sofia bid ev2: %v", err)
	}
	// Resolve ev2: Sofia (0 nights) beats Carlos (1 night) despite bidding later!
	if _, err := st.RunResolver(ctx, baseNow, ev2.ID); err != nil {
		t.Fatalf("RunResolver ev2: %v", err)
	}
	ev2Resolved, err := st.GetEventView(ctx, ev2.ID, newbie.ID, false)
	if err != nil {
		t.Fatalf("GetEventView ev2: %v", err)
	}
	if ev2Resolved.WinnerTeacherID == nil || *ev2Resolved.WinnerTeacherID != newbie.ID || ev2Resolved.ResolvedVia != "fairness" {
		t.Fatalf("expected Sofia to win ev2 via fairness, got winner=%v via=%s", ev2Resolved.WinnerName, ev2Resolved.ResolvedVia)
	}
	if ev2Resolved.FBEventCopy == "" {
		t.Fatalf("expected FBEventCopy to be populated on resolved event")
	}

	// 3. Veto Token override on ev3: give Carlos another confirmed night first or let him use a token.
	// Even if Carlos bids with a token and Sofia bids normal, Carlos's token wins!
	if err := st.PlaceBid(ctx, newbie.ID, ev3.ID, "normal", baseNow); err != nil {
		t.Fatalf("Sofia bid ev3: %v", err)
	}
	if err := st.PlaceBid(ctx, reg.ID, ev3.ID, "token", baseNow.Add(time.Hour)); err != nil {
		t.Fatalf("Carlos token bid ev3: %v", err)
	}
	if _, err := st.RunResolver(ctx, baseNow, ev3.ID); err != nil {
		t.Fatalf("RunResolver ev3: %v", err)
	}
	ev3Resolved, _ := st.GetEventView(ctx, ev3.ID, reg.ID, false)
	if ev3Resolved.WinnerTeacherID == nil || *ev3Resolved.WinnerTeacherID != reg.ID || ev3Resolved.ResolvedVia != "token" {
		t.Fatalf("expected Carlos to win ev3 via token, got winner=%s via=%s", ev3Resolved.WinnerName, ev3Resolved.ResolvedVia)
	}
	availAfter1, totalTokens, err := st.AvailableTokens(ctx, reg.ID, 2026)
	if err != nil || availAfter1 != 1 || totalTokens != 2 {
		t.Fatalf("expected 1/2 tokens left for Carlos, got %d/%d (err=%v)", availAfter1, totalTokens, err)
	}

	// 4. Help flag surfaces requester name + allows volunteering
	if err := st.PlaceBid(ctx, newbie.ID, ev4.ID, "normal", baseNow); err != nil {
		t.Fatalf("Sofia bid ev4: %v", err)
	}
	if err := st.UpdateEventDetails(ctx, newbie, ev4.ID, "bachata", "Sensual footwork", "DJ Papi", "Bring water", true, false, false); err != nil {
		t.Fatalf("UpdateEventDetails: %v", err)
	}
	ev4ForCarlos, _ := st.GetEventView(ctx, ev4.ID, reg.ID, false)
	if !ev4ForCarlos.NeedLead || ev4ForCarlos.HelpRequester != "Sofia New" {
		t.Fatalf("expected help flag to expose Sofia New as HelpRequester, got %+v", ev4ForCarlos)
	}
	if err := st.VolunteerForEvent(ctx, admin, ev4.ID, "lead"); err != nil {
		t.Fatalf("VolunteerForEvent: %v", err)
	}
	ev4AfterVol, _ := st.GetEventView(ctx, ev4.ID, reg.ID, false)
	if ev4AfterVol.NeedLead || len(ev4AfterVol.Volunteers) != 1 {
		t.Fatalf("expected NeedLead cleared and 1 volunteer recorded, got %+v", ev4AfterVol)
	}

	// 5. OTP login flow
	code, err := st.RequestOTP(ctx, "9705550102", baseNow)
	if err != nil || len(code) != 6 {
		t.Fatalf("RequestOTP: code=%q err=%v", code, err)
	}
	sessTok, loggedIn, err := st.VerifyOTP(ctx, "+19705550102", code, baseNow.Add(time.Minute))
	if err != nil || sessTok == "" || loggedIn.ID != newbie.ID {
		t.Fatalf("VerifyOTP: tok=%q err=%v", sessTok, err)
	}
}
