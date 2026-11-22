package store

import (
	"context"
	"database/sql"
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

	// 4. Help flag surfaces requester name + allows volunteering (but NOT self-volunteering, and NOT editing by others)
	if err := st.PlaceBid(ctx, newbie.ID, ev4.ID, "normal", baseNow); err != nil {
		t.Fatalf("Sofia bid ev4: %v", err)
	}
	if err := st.UpdateEventDetails(ctx, reg, ev4.ID, "salsa", "Hijacked", "DJ Bad", "", false, false, false); err == nil {
		t.Fatalf("expected non-owner Carlos to be rejected from editing Sofia's event")
	}
	if err := st.UpdateEventDetails(ctx, newbie, ev4.ID, "bachata", "Sensual footwork", "DJ Papi", "Bring water", true, false, false); err != nil {
		t.Fatalf("UpdateEventDetails: %v", err)
	}
	if err := st.VolunteerForEvent(ctx, newbie, ev4.ID, "lead"); err == nil {
		t.Fatalf("expected Sofia to be blocked from volunteering on her own lesson")
	}
	ev4ForCarlos, _ := st.GetEventView(ctx, ev4.ID, reg.ID, false)
	if !ev4ForCarlos.NeedLead || ev4ForCarlos.HelpRequester != "Sofia New" || !ev4ForCarlos.CanVolunteer {
		t.Fatalf("expected help flag to expose Sofia New as HelpRequester and allow Carlos to volunteer, got %+v", ev4ForCarlos)
	}
	if err := st.VolunteerForEvent(ctx, admin, ev4.ID, "lead"); err != nil {
		t.Fatalf("VolunteerForEvent: %v", err)
	}
	ev4AfterVol, _ := st.GetEventView(ctx, ev4.ID, reg.ID, false)
	if ev4AfterVol.NeedLead || len(ev4AfterVol.Volunteers) != 1 {
		t.Fatalf("expected NeedLead cleared and 1 volunteer recorded, got %+v", ev4AfterVol)
	}
	if err := st.UnvolunteerForEvent(ctx, admin, ev4.ID, admin.ID, "lead"); err != nil {
		t.Fatalf("UnvolunteerForEvent: %v", err)
	}
	ev4AfterUnvol, _ := st.GetEventView(ctx, ev4.ID, reg.ID, false)
	if !ev4AfterUnvol.NeedLead || len(ev4AfterUnvol.Volunteers) != 0 {
		t.Fatalf("expected NeedLead re-opened and 0 volunteers after unvolunteer, got %+v", ev4AfterUnvol)
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

func TestLegacySchemaMigrationAndEmailInviteExpiry(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	// Simulate pre-email database schema with NOT NULL UNIQUE phone.
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = rawDB.Exec(`
		CREATE TABLE teachers (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			phone TEXT NOT NULL UNIQUE,
			handle TEXT NOT NULL DEFAULT '',
			is_admin INTEGER NOT NULL DEFAULT 0,
			active INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		INSERT INTO teachers(name, phone, is_admin, active) VALUES ('Admin', '+19705550100', 1, 1);
		CREATE TABLE otp_codes (phone TEXT NOT NULL, code TEXT NOT NULL, expires_at TEXT NOT NULL);
	`)
	rawDB.Close()
	if err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}

	st, err := Open(dbPath, "https://scrumpys.codeciech.com", "manual", "")
	if err != nil {
		t.Fatalf("Open migrated DB: %v", err)
	}
	defer st.Close()

	now := time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC)

	// Attach email to existing phone-bootstrapped admin in place (keeps ID=1).
	adm, err := st.SaveTeacher(ctx, "Admin", "admin@codeciech.com", "+19705550100", "", true, 2026)
	if err != nil || adm.ID != 1 || adm.Email != "admin@codeciech.com" {
		t.Fatalf("expected in-place admin email upgrade on ID=1, got %+v err=%v", adm, err)
	}

	// Mint 7-day invite link and verify deferred signup (no roster row before VerifyOTP).
	inv, err := st.GetInviteStatus(ctx, now)
	if err != nil || !inv.Active || inv.Token == "" {
		t.Fatalf("GetInviteStatus: %+v err=%v", inv, err)
	}

	code1, err := st.RequestSignupOTP(ctx, "Lucia@Example.com", "Lucia Dancer", "", inv.Token, now)
	if err != nil || len(code1) != 6 {
		t.Fatalf("RequestSignupOTP: code=%q err=%v", code1, err)
	}
	beforeVerify, _ := st.ListTeachers(ctx, now)
	if len(beforeVerify) != 1 {
		t.Fatalf("expected roster size 1 before OTP verification, got %d", len(beforeVerify))
	}

	_, lucia, err := st.VerifyOTP(ctx, "lucia@example.com", code1, now.Add(time.Minute))
	if err != nil || lucia.Email != "lucia@example.com" {
		t.Fatalf("VerifyOTP signup: teacher=%+v err=%v", lucia, err)
	}

	// Second email-only instructor with blank phone must not collide on empty phone.
	code2, err := st.RequestSignupOTP(ctx, "mateo@example.com", "Mateo", "", inv.Token, now)
	if err != nil {
		t.Fatalf("RequestSignupOTP 2: %v", err)
	}
	if _, _, err := st.VerifyOTP(ctx, "mateo@example.com", code2, now.Add(time.Minute)); err != nil {
		t.Fatalf("VerifyOTP 2 (blank phone uniqueness): %v", err)
	}

	// Advance clock past 7-day TTL -> invite link automatically expires.
	expiredAt := now.AddDate(0, 0, 8)
	if _, err := st.RequestSignupOTP(ctx, "late@example.com", "Late", "", inv.Token, expiredAt); err != ErrInvalidInvite {
		t.Fatalf("expected ErrInvalidInvite after 7d expiry, got %v", err)
	}

	// Mint fresh link -> works; revoke -> immediately rejected.
	inv2, err := st.RotateInviteToken(ctx, expiredAt, 7)
	if err != nil || !inv2.Active {
		t.Fatalf("RotateInviteToken: %+v err=%v", inv2, err)
	}
	if err := st.RevokeInviteToken(ctx); err != nil {
		t.Fatalf("RevokeInviteToken: %v", err)
	}
	if _, err := st.RequestSignupOTP(ctx, "revoked@example.com", "Revoked", "", inv2.Token, expiredAt); err != ErrInvalidInvite {
		t.Fatalf("expected ErrInvalidInvite after revoke, got %v", err)
	}
}
