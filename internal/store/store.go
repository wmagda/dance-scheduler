// Package store implements the SQLite persistence and core fairness/token
// allocation engine for the Thursday Latin Dancing instructor scheduler.
package store

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/wojtek/dance-scheduler/internal/transport"
)

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS config (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS teachers (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	name          TEXT NOT NULL,
	phone         TEXT NOT NULL UNIQUE,
	handle        TEXT NOT NULL DEFAULT '',
	is_admin      INTEGER NOT NULL DEFAULT 0,
	active        INTEGER NOT NULL DEFAULT 1,
	created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS events (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	date              TEXT NOT NULL UNIQUE, -- YYYY-MM-DD (Thursday)
	status            TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','tentative','recruiting','resolved','done')),
	winner_teacher_id INTEGER REFERENCES teachers(id),
	resolved_via      TEXT NOT NULL DEFAULT '',
	lesson_type       TEXT NOT NULL DEFAULT 'salsa',
	lesson_detail     TEXT NOT NULL DEFAULT '',
	dj                TEXT NOT NULL DEFAULT '',
	notes             TEXT NOT NULL DEFAULT '',
	need_lead         INTEGER NOT NULL DEFAULT 0,
	need_follow       INTEGER NOT NULL DEFAULT 0,
	need_co           INTEGER NOT NULL DEFAULT 0,
	escalated         INTEGER NOT NULL DEFAULT 0,
	resolved_at       TEXT,
	created_at        TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS bids (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	event_id     INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
	teacher_id   INTEGER NOT NULL REFERENCES teachers(id) ON DELETE CASCADE,
	kind         TEXT NOT NULL DEFAULT 'normal' CHECK (kind IN ('normal','token')),
	created_at   TEXT NOT NULL DEFAULT (datetime('now')),
	withdrawn_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_bids_event ON bids(event_id);

CREATE TABLE IF NOT EXISTS tokens (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	teacher_id     INTEGER NOT NULL REFERENCES teachers(id) ON DELETE CASCADE,
	year           INTEGER NOT NULL,
	reason         TEXT NOT NULL DEFAULT 'annual',
	used_event_id  INTEGER REFERENCES events(id),
	used_at        TEXT,
	created_at     TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_tokens_teacher_year ON tokens(teacher_id, year);

CREATE TABLE IF NOT EXISTS confirmations (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	teacher_id INTEGER NOT NULL REFERENCES teachers(id) ON DELETE CASCADE,
	event_id   INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
	role       TEXT NOT NULL CHECK (role IN ('teacher','lead','follow','co')),
	created_at TEXT NOT NULL DEFAULT (datetime('now')),
	UNIQUE(teacher_id, event_id, role)
);

CREATE TABLE IF NOT EXISTS otp_codes (
	phone      TEXT NOT NULL,
	code       TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS sessions (
	token      TEXT PRIMARY KEY,
	teacher_id INTEGER NOT NULL REFERENCES teachers(id) ON DELETE CASCADE,
	expires_at TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS outbox (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	channel    TEXT NOT NULL,
	recipient  TEXT NOT NULL,
	body       TEXT NOT NULL,
	created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
`

var defaultConfig = map[string]string{
	"fairness_window_days": "90",
	"resolve_days_before":  "7",
	"recruit_days_before":  "14",
	"tokens_per_year":      "2",
	"months_open_ahead":    "6",
}

type Store struct {
	db      *sql.DB
	hub     transport.Transport
	baseURL string
}

type Teacher struct {
	ID              int64      `json:"id"`
	Name            string     `json:"name"`
	Phone           string     `json:"phone"`
	Handle          string     `json:"handle"`
	IsAdmin         bool       `json:"is_admin"`
	Active          bool       `json:"active"`
	FairnessScore   int        `json:"fairness_score"`
	TokensAvailable int        `json:"tokens_available"`
	TokensTotal     int        `json:"tokens_total"`
	TokensUsed      []TokenUse `json:"tokens_used,omitempty"`
}

type TokenUse struct {
	TokenID   int64  `json:"token_id"`
	TeacherID int64  `json:"teacher_id"`
	Teacher   string `json:"teacher"`
	Year      int    `json:"year"`
	Reason    string `json:"reason"`
	EventDate string `json:"event_date"`
	UsedAt    string `json:"used_at"`
}

type VolunteerView struct {
	TeacherID int64  `json:"teacher_id"`
	Name      string `json:"name"`
	Role      string `json:"role"`
}

type BidView struct {
	ID          int64  `json:"id"`
	TeacherID   int64  `json:"teacher_id"`
	TeacherName string `json:"teacher_name"`
	Kind        string `json:"kind"`
	CreatedAt   string `json:"created_at"`
	Fairness    int    `json:"fairness"`
}

type EventView struct {
	ID              int64           `json:"id"`
	Date            string          `json:"date"`
	ResolveDeadline string          `json:"resolve_deadline"`
	Status          string          `json:"status"`
	WinnerTeacherID *int64          `json:"winner_teacher_id,omitempty"`
	WinnerName      string          `json:"winner_name,omitempty"`
	ResolvedVia     string          `json:"resolved_via,omitempty"`
	LessonType      string          `json:"lesson_type"`
	LessonDetail    string          `json:"lesson_detail"`
	DJ              string          `json:"dj"`
	Notes           string          `json:"notes"`
	NeedLead        bool            `json:"need_lead"`
	NeedFollow      bool            `json:"need_follow"`
	NeedCo          bool            `json:"need_co"`
	HelpRequester   string          `json:"help_requester,omitempty"`
	MyBidKind       string          `json:"my_bid_kind,omitempty"` // "", "normal", "token"
	CanEditDetails  bool            `json:"can_edit_details"`
	CanVolunteer    bool            `json:"can_volunteer"`
	Volunteers      []VolunteerView `json:"volunteers"`
	FBEventCopy     string          `json:"fb_event_copy,omitempty"`
	AdminBids       []BidView       `json:"admin_bids,omitempty"` // populated for admin only
}

type OutboxEntry struct {
	ID        int64  `json:"id"`
	Channel   string `json:"channel"`
	Recipient string `json:"recipient"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

func Open(dbPath, baseURL, transportMode, webhookURL string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	s := &Store{
		db:      db,
		baseURL: strings.TrimRight(baseURL, "/"),
	}
	s.hub = transport.NewHub(transportMode, webhookURL, s.RecordOutbox)

	for k, v := range defaultConfig {
		if _, err := db.Exec(`INSERT OR IGNORE INTO config(key, value) VALUES (?, ?)`, k, v); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	_, _ = db.Exec(`UPDATE events SET dj = '' WHERE dj = 'TBD'`)
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Hub() transport.Transport { return s.hub }

var nonDigitsRe = regexp.MustCompile(`[^0-9+]`)

func NormalizePhone(p string) string {
	p = strings.TrimSpace(nonDigitsRe.ReplaceAllString(p, ""))
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "+") {
		if len(p) == 10 {
			return "+1" + p
		}
		return "+" + p
	}
	return p
}

func (s *Store) RecordOutbox(ctx context.Context, ch transport.Channel, recipient, body string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO outbox(channel, recipient, body) VALUES (?, ?, ?)`,
		string(ch), recipient, body)
	return err
}

func (s *Store) ListOutbox(ctx context.Context, limit int) ([]OutboxEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, channel, recipient, body, created_at FROM outbox ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxEntry
	for rows.Next() {
		var e OutboxEntry
		if err := rows.Scan(&e.ID, &e.Channel, &e.Recipient, &e.Body, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) GetConfigMap(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM config`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[string]string)
	for k, v := range defaultConfig {
		m[k] = v
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

func (s *Store) GetConfigInt(ctx context.Context, key string, fallback int) int {
	var val string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM config WHERE key = ?`, key).Scan(&val)
	if err != nil {
		return fallback
	}
	n, err := strconv.Atoi(strings.TrimSpace(val))
	if err != nil {
		return fallback
	}
	return n
}

func (s *Store) UpdateConfig(ctx context.Context, updates map[string]string) error {
	for k, v := range updates {
		if _, ok := defaultConfig[k]; !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 0 {
			return fmt.Errorf("invalid integer for %s", k)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO config(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			k, strconv.Itoa(n)); err != nil {
			return err
		}
	}
	return nil
}

// CreateTeacher adds or reactivates a teacher in the roster and ensures their
// annual veto token allocation for the current year.
func (s *Store) CreateTeacher(ctx context.Context, name, phone, handle string, isAdmin bool, year int) (*Teacher, error) {
	name = strings.TrimSpace(name)
	phone = NormalizePhone(phone)
	if name == "" || phone == "" {
		return nil, errors.New("name and phone are required")
	}
	adminInt := 0
	if isAdmin {
		adminInt = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO teachers(name, phone, handle, is_admin, active)
		VALUES (?, ?, ?, ?, 1)
		ON CONFLICT(phone) DO UPDATE SET
			name = excluded.name,
			handle = CASE WHEN excluded.handle != '' THEN excluded.handle ELSE teachers.handle END,
			is_admin = CASE WHEN excluded.is_admin = 1 THEN 1 ELSE teachers.is_admin END,
			active = 1
	`, name, phone, strings.TrimSpace(handle), adminInt)
	if err != nil {
		return nil, err
	}
	t, err := s.GetTeacherByPhone(ctx, phone)
	if err != nil {
		return nil, err
	}
	if err := s.EnsureYearTokens(ctx, t.ID, year); err != nil {
		return nil, err
	}
	return s.GetTeacherByID(ctx, t.ID, time.Now().UTC())
}

func (s *Store) RemoveTeacher(ctx context.Context, teacherID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE teachers SET active = 0 WHERE id = ?`, teacherID)
	return err
}

// GetInviteToken returns the current self-signup invite token, generating one
// on first use. The token is shared via a link posted in the WhatsApp group,
// so possession of the token approximates group membership.
func (s *Store) GetInviteToken(ctx context.Context) (string, error) {
	var val string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM config WHERE key = 'invite_token'`).Scan(&val)
	if err == sql.ErrNoRows || (err == nil && strings.TrimSpace(val) == "") {
		return s.RotateInviteToken(ctx)
	}
	return val, err
}

// RotateInviteToken replaces the invite token, invalidating previously shared
// signup links.
func (s *Store) RotateInviteToken(ctx context.Context) (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO config(key, value) VALUES ('invite_token', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, tok); err != nil {
		return "", err
	}
	return tok, nil
}

// ValidateInviteToken reports whether the supplied signup token matches the
// current one (constant-time compare).
func (s *Store) ValidateInviteToken(ctx context.Context, token string) bool {
	tok, err := s.GetInviteToken(ctx)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), []byte(tok)) == 1
}

func (s *Store) EnsureYearTokens(ctx context.Context, teacherID int64, year int) error {
	if year <= 0 {
		year = time.Now().UTC().Year()
	}
	perYear := s.GetConfigInt(ctx, "tokens_per_year", 2)
	var existing int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tokens WHERE teacher_id = ? AND year = ? AND reason = 'annual'`,
		teacherID, year).Scan(&existing); err != nil {
		return err
	}
	for i := existing; i < perYear; i++ {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO tokens(teacher_id, year, reason) VALUES (?, ?, 'annual')`,
			teacherID, year); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) GrantExtraToken(ctx context.Context, teacherID int64, year int, reason string) error {
	if year <= 0 {
		year = time.Now().UTC().Year()
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "admin bonus"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens(teacher_id, year, reason) VALUES (?, ?, ?)`,
		teacherID, year, reason)
	return err
}

// AvailableTokens returns how many unspent tokens a teacher has for a given year,
// minus any tokens currently locked in active unresolved token bids for that year.
func (s *Store) AvailableTokens(ctx context.Context, teacherID int64, year int) (available int, total int, err error) {
	if err := s.EnsureYearTokens(ctx, teacherID, year); err != nil {
		return 0, 0, err
	}
	var unspent int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tokens WHERE teacher_id = ? AND year = ? AND used_event_id IS NULL`,
		teacherID, year).Scan(&unspent); err != nil {
		return 0, 0, err
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tokens WHERE teacher_id = ? AND year = ?`,
		teacherID, year).Scan(&total); err != nil {
		return 0, 0, err
	}
	yearPrefix := fmt.Sprintf("%04d-", year)
	var lockedInBids int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM bids b
		JOIN events e ON e.id = b.event_id
		WHERE b.teacher_id = ? AND b.kind = 'token' AND b.withdrawn_at IS NULL
		  AND e.status IN ('open','tentative','recruiting')
		  AND e.date LIKE ?
	`, teacherID, yearPrefix+"%").Scan(&lockedInBids); err != nil {
		return 0, 0, err
	}
	avail := unspent - lockedInBids
	if avail < 0 {
		avail = 0
	}
	return avail, total, nil
}

// FairnessScore computes the number of confirmed primary teaching nights for
// teacherID in [refDate - fairness_window_days, refDate]. Equal start (0) at launch.
func (s *Store) FairnessScore(ctx context.Context, teacherID int64, refDate time.Time) (int, error) {
	windowDays := s.GetConfigInt(ctx, "fairness_window_days", 90)
	start := refDate.AddDate(0, 0, -windowDays).Format("2006-01-02")
	end := refDate.Format("2006-01-02")
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM confirmations c
		JOIN events e ON e.id = c.event_id
		WHERE c.teacher_id = ? AND c.role = 'teacher'
		  AND e.date >= ? AND e.date <= ?
	`, teacherID, start, end).Scan(&count)
	return count, err
}

func (s *Store) GetTeacherByPhone(ctx context.Context, phone string) (*Teacher, error) {
	phone = NormalizePhone(phone)
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM teachers WHERE phone = ? AND active = 1`, phone).Scan(&id)
	if err != nil {
		return nil, err
	}
	return s.GetTeacherByID(ctx, id, time.Now().UTC())
}

func (s *Store) GetTeacherByID(ctx context.Context, id int64, refTime time.Time) (*Teacher, error) {
	var t Teacher
	var isAdmin, active int
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, phone, handle, is_admin, active FROM teachers WHERE id = ?`, id,
	).Scan(&t.ID, &t.Name, &t.Phone, &t.Handle, &isAdmin, &active)
	if err != nil {
		return nil, err
	}
	t.IsAdmin = isAdmin == 1
	t.Active = active == 1
	score, err := s.FairnessScore(ctx, t.ID, refTime)
	if err != nil {
		return nil, err
	}
	t.FairnessScore = score
	avail, total, err := s.AvailableTokens(ctx, t.ID, refTime.Year())
	if err != nil {
		return nil, err
	}
	t.TokensAvailable = avail
	t.TokensTotal = total
	return &t, nil
}

func (s *Store) ListTeachers(ctx context.Context, refTime time.Time) ([]Teacher, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM teachers WHERE active = 1 ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()

	var out []Teacher
	for _, id := range ids {
		t, err := s.GetTeacherByID(ctx, id, refTime)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, nil
}

func (s *Store) ListUsedTokens(ctx context.Context) ([]TokenUse, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT tk.id, tk.teacher_id, t.name, tk.year, tk.reason, e.date, COALESCE(tk.used_at, '')
		FROM tokens tk
		JOIN teachers t ON t.id = tk.teacher_id
		JOIN events e ON e.id = tk.used_event_id
		ORDER BY e.date DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenUse
	for rows.Next() {
		var u TokenUse
		if err := rows.Scan(&u.TokenID, &u.TeacherID, &u.Teacher, &u.Year, &u.Reason, &u.EventDate, &u.UsedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// EnsureUpcomingThursdays populates Thursday dates from fromDate out to
// months_open_ahead months. Quietly idempotent (no group spam on open).
func (s *Store) EnsureUpcomingThursdays(ctx context.Context, fromDate time.Time) error {
	months := s.GetConfigInt(ctx, "months_open_ahead", 6)
	start := time.Date(fromDate.Year(), fromDate.Month(), fromDate.Day(), 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, months, 0)
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		if d.Weekday() == time.Thursday {
			dateStr := d.Format("2006-01-02")
			if _, err := s.db.ExecContext(ctx,
				`INSERT OR IGNORE INTO events(date, status) VALUES (?, 'open')`, dateStr); err != nil {
				return err
			}
		}
	}
	return nil
}

// PlaceBid submits or upgrades a teacher's bid on an unresolved Thursday date.
// Enforces privacy (never reveals competitor names) and sends private DM status.
func (s *Store) PlaceBid(ctx context.Context, teacherID int64, eventID int64, kind string, now time.Time) error {
	if kind != "normal" && kind != "token" {
		return errors.New("kind must be 'normal' or 'token'")
	}
	var eventDate, status string
	err := s.db.QueryRowContext(ctx, `SELECT date, status FROM events WHERE id = ?`, eventID).Scan(&eventDate, &status)
	if err != nil {
		return fmt.Errorf("event not found: %w", err)
	}
	if status == "resolved" || status == "done" {
		return errors.New("this date is already resolved")
	}
	evTime, err := time.Parse("2006-01-02", eventDate)
	if err != nil {
		return err
	}

	// Check if teacher already has an active bid on this event.
	var existingID int64
	var existingKind string
	err = s.db.QueryRowContext(ctx,
		`SELECT id, kind FROM bids WHERE event_id = ? AND teacher_id = ? AND withdrawn_at IS NULL`,
		eventID, teacherID).Scan(&existingID, &existingKind)
	hasExisting := err == nil

	if kind == "token" && (!hasExisting || existingKind != "token") {
		avail, _, err := s.AvailableTokens(ctx, teacherID, evTime.Year())
		if err != nil {
			return err
		}
		if avail <= 0 {
			return fmt.Errorf("no veto tokens remaining for %d", evTime.Year())
		}
	}

	if hasExisting {
		if existingKind == kind {
			return nil // idempotent
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE bids SET kind = ?, created_at = ? WHERE id = ?`,
			kind, now.UTC().Format(time.RFC3339), existingID); err != nil {
			return err
		}
	} else {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO bids(event_id, teacher_id, kind, created_at) VALUES (?, ?, ?, ?)`,
			eventID, teacherID, kind, now.UTC().Format(time.RFC3339)); err != nil {
			return err
		}
	}

	if status == "open" || status == "recruiting" {
		if _, err := s.db.ExecContext(ctx, `UPDATE events SET status = 'tentative' WHERE id = ?`, eventID); err != nil {
			return err
		}
	}

	// Notify bidders privately according to the noise & privacy policy.
	resolveDays := s.GetConfigInt(ctx, "resolve_days_before", 7)
	deadline := evTime.AddDate(0, 0, -resolveDays).Format("2006-01-02")

	if kind == "token" {
		t, err := s.GetTeacherByID(ctx, teacherID, now)
		if err == nil {
			_ = s.hub.PostToGroup(ctx, fmt.Sprintf("⭐ %s placed a Veto Token reservation on Thursday %s (resolves %s).", t.Name, eventDate, deadline))
		}
	}

	ranked, err := s.rankActiveBids(ctx, eventID, evTime)
	if err != nil {
		return err
	}
	if len(ranked) == 1 {
		b := ranked[0]
		_ = s.hub.SendDM(ctx, b.recipient,
			fmt.Sprintf("You hold Thursday %s tentatively (%s bid) — it can still be outbid until %s. Edit your lesson/DJ details: %s",
				eventDate, b.Kind, deadline, s.baseURL))
	} else if len(ranked) > 1 {
		for idx, b := range ranked {
			standing := "trailing on fairness/token priority"
			if idx == 0 {
				standing = "currently in 1st place to win"
			}
			_ = s.hub.SendDM(ctx, b.recipient,
				fmt.Sprintf("Thursday %s now has multiple bids and will resolve on %s. You are %s. Manage your bids: %s",
					eventDate, deadline, standing, s.baseURL))
		}
	}
	return nil
}

func (s *Store) WithdrawBid(ctx context.Context, teacherID, eventID int64, now time.Time) error {
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM events WHERE id = ?`, eventID).Scan(&status); err != nil {
		return err
	}
	if status == "resolved" || status == "done" {
		return errors.New("cannot withdraw from a resolved date")
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE bids SET withdrawn_at = ? WHERE event_id = ? AND teacher_id = ? AND withdrawn_at IS NULL`,
		now.UTC().Format(time.RFC3339), eventID, teacherID)
	if err != nil {
		return err
	}
	var remaining int
	_ = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM bids WHERE event_id = ? AND withdrawn_at IS NULL`, eventID).Scan(&remaining)
	if remaining == 0 {
		_, _ = s.db.ExecContext(ctx, `UPDATE events SET status = 'open' WHERE id = ?`, eventID)
	}
	return nil
}

type rankedBid struct {
	BidView
	recipient transport.Recipient
	reason    string
}

// rankActiveBids orders active bids on an event by:
// 1. Token bid ('token') beats 'normal'
// 2. Lowest FairnessScore (fewest teaching nights in rolling window before eventDate)
// 3. Earliest bid created_at
// 4. Lowest bid ID
func (s *Store) rankActiveBids(ctx context.Context, eventID int64, eventDate time.Time) ([]rankedBid, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.id, b.teacher_id, t.name, t.phone, t.handle, b.kind, b.created_at
		FROM bids b
		JOIN teachers t ON t.id = b.teacher_id
		WHERE b.event_id = ? AND b.withdrawn_at IS NULL AND t.active = 1
	`, eventID)
	if err != nil {
		return nil, err
	}
	var list []rankedBid
	for rows.Next() {
		var rb rankedBid
		if err := rows.Scan(&rb.ID, &rb.TeacherID, &rb.TeacherName, &rb.recipient.Phone, &rb.recipient.Handle, &rb.Kind, &rb.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		rb.recipient.TeacherID = rb.TeacherID
		rb.recipient.Name = rb.TeacherName
		list = append(list, rb)
	}
	rows.Close()

	for i := range list {
		score, err := s.FairnessScore(ctx, list[i].TeacherID, eventDate)
		if err != nil {
			return nil, err
		}
		list[i].Fairness = score
	}

	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.Kind != b.Kind {
			return a.Kind == "token"
		}
		if a.Fairness != b.Fairness {
			return a.Fairness < b.Fairness
		}
		if a.CreatedAt != b.CreatedAt {
			return a.CreatedAt < b.CreatedAt
		}
		return a.ID < b.ID
	})

	if len(list) > 0 {
		top := &list[0]
		switch {
		case top.Kind == "token":
			top.reason = "token"
		case len(list) == 1:
			top.reason = "sole_bidder"
		case list[0].Fairness < list[1].Fairness:
			top.reason = "fairness"
		default:
			top.reason = "earliest_bid"
		}
	}
	return list, nil
}

// RunResolver resolves all tentative events whose resolution deadline (date - resolve_days_before)
// has arrived as of `now`, or force-resolves a single event when forceEventID > 0.
func (s *Store) RunResolver(ctx context.Context, now time.Time, forceEventID int64) (int, error) {
	resolveDays := s.GetConfigInt(ctx, "resolve_days_before", 7)
	cutoffDate := now.UTC().AddDate(0, 0, resolveDays).Format("2006-01-02")

	var rows *sql.Rows
	var err error
	if forceEventID > 0 {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, date FROM events WHERE id = ? AND status IN ('open','tentative','recruiting')`, forceEventID)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, date FROM events WHERE status IN ('tentative','recruiting') AND date <= ? ORDER BY date ASC`, cutoffDate)
	}
	if err != nil {
		return 0, err
	}
	type evRow struct {
		id   int64
		date string
	}
	var targets []evRow
	for rows.Next() {
		var r evRow
		if err := rows.Scan(&r.id, &r.date); err != nil {
			rows.Close()
			return 0, err
		}
		targets = append(targets, r)
	}
	rows.Close()

	resolvedCount := 0
	for _, ev := range targets {
		evTime, err := time.Parse("2006-01-02", ev.date)
		if err != nil {
			continue
		}
		ranked, err := s.rankActiveBids(ctx, ev.id, evTime)
		if err != nil {
			return resolvedCount, err
		}
		if len(ranked) == 0 {
			continue
		}
		winner := ranked[0]
		nowStr := now.UTC().Format(time.RFC3339)

		if winner.Kind == "token" {
			var tokID int64
			err := s.db.QueryRowContext(ctx,
				`SELECT id FROM tokens WHERE teacher_id = ? AND year = ? AND used_event_id IS NULL ORDER BY id ASC LIMIT 1`,
				winner.TeacherID, evTime.Year()).Scan(&tokID)
			if err == nil {
				_, _ = s.db.ExecContext(ctx,
					`UPDATE tokens SET used_event_id = ?, used_at = ? WHERE id = ?`,
					ev.id, nowStr, tokID)
			}
		}

		if _, err := s.db.ExecContext(ctx, `
			UPDATE events
			SET status = 'resolved', winner_teacher_id = ?, resolved_via = ?, resolved_at = ?
			WHERE id = ?
		`, winner.TeacherID, winner.reason, nowStr, ev.id); err != nil {
			return resolvedCount, err
		}

		if _, err := s.db.ExecContext(ctx, `
			INSERT OR IGNORE INTO confirmations(teacher_id, event_id, role)
			VALUES (?, ?, 'teacher')
		`, winner.TeacherID, ev.id); err != nil {
			return resolvedCount, err
		}

		openAlts := s.nextOpenDates(ctx, now.Format("2006-01-02"), 3)
		altStr := strings.Join(openAlts, ", ")
		if altStr == "" {
			altStr = "check calendar"
		}

		_ = s.hub.SendDM(ctx, winner.recipient,
			fmt.Sprintf("🎉 Confirmed! You are teaching Thursday %s at Scrumpy's (resolved via %s). Please confirm your lesson type, DJ, and any lead/follow help needed: %s",
				ev.date, winner.reason, s.baseURL))

		for _, loser := range ranked[1:] {
			_ = s.hub.SendDM(ctx, loser.recipient,
				fmt.Sprintf("Thursday %s has resolved to another instructor (via %s). Grab another open Thursday (%s): %s",
					ev.date, winner.reason, altStr, s.baseURL))
		}

		// Announce resolution to the group (only resolutions & help flags go to group).
		evView, err := s.GetEventView(ctx, ev.id, 0, true)
		if err == nil {
			djLabel := evView.DJ
			if djLabel == "" {
				djLabel = "TBD"
			}
			groupMsg := fmt.Sprintf("✅ Resolved for Thursday %s: %s is teaching (%s, DJ: %s).",
				ev.date, winner.TeacherName, strings.Title(evView.LessonType), djLabel)
			if winner.Kind == "token" {
				groupMsg += " [Reserved with Veto Token ⭐]"
			}
			_ = s.hub.PostToGroup(ctx, groupMsg)
			_ = s.RecordOutbox(ctx, transport.ChannelFBEvent, "Facebook Event Copy", evView.FBEventCopy)
		}

		resolvedCount++
	}
	return resolvedCount, nil
}

// RunRecruitment checks for open/unbid Thursdays within recruit_days_before (T-14)
// and escalates to admins at resolve_days_before (T-7).
func (s *Store) RunRecruitment(ctx context.Context, now time.Time) (recruited int, escalated int, err error) {
	recruitDays := s.GetConfigInt(ctx, "recruit_days_before", 14)
	resolveDays := s.GetConfigInt(ctx, "resolve_days_before", 7)
	todayStr := now.UTC().Format("2006-01-02")
	recruitCutoff := now.UTC().AddDate(0, 0, recruitDays).Format("2006-01-02")
	escalateCutoff := now.UTC().AddDate(0, 0, resolveDays).Format("2006-01-02")

	// 1. T-14 Recruitment for open dates with 0 active bids
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.date FROM events e
		WHERE e.status = 'open' AND e.date >= ? AND e.date <= ?
		  AND NOT EXISTS (SELECT 1 FROM bids b WHERE b.event_id = e.id AND b.withdrawn_at IS NULL)
	`, todayStr, recruitCutoff)
	if err != nil {
		return 0, 0, err
	}
	type evItem struct {
		id   int64
		date string
	}
	var toRecruit []evItem
	for rows.Next() {
		var it evItem
		if err := rows.Scan(&it.id, &it.date); err != nil {
			rows.Close()
			return 0, 0, err
		}
		toRecruit = append(toRecruit, it)
	}
	rows.Close()

	teachers, err := s.ListTeachers(ctx, now)
	if err != nil {
		return 0, 0, err
	}
	sort.SliceStable(teachers, func(i, j int) bool {
		return teachers[i].FairnessScore < teachers[j].FairnessScore
	})

	for _, ev := range toRecruit {
		_, _ = s.db.ExecContext(ctx, `UPDATE events SET status = 'recruiting' WHERE id = ?`, ev.id)
		_ = s.hub.PostToGroup(ctx,
			fmt.Sprintf("📣 Open Thursday %s still needs an instructor! Claim it here: %s", ev.date, s.baseURL))
		limit := 5
		if len(teachers) < limit {
			limit = len(teachers)
		}
		for _, t := range teachers[:limit] {
			_ = s.hub.SendDM(ctx, transport.Recipient{
				TeacherID: t.ID, Name: t.Name, Phone: t.Phone, Handle: t.Handle,
			}, fmt.Sprintf("Hey %s — Thursday %s is still open and you're at the top of the fairness priority list (%d nights in the last 90d). Want to teach? Claim it: %s",
				t.Name, ev.date, t.FairnessScore, s.baseURL))
		}
		recruited++
	}

	// 2. T-7 Admin Escalation for still-unclaimed dates
	rows2, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.date FROM events e
		WHERE e.status IN ('open','recruiting') AND e.escalated = 0
		  AND e.date >= ? AND e.date <= ?
		  AND NOT EXISTS (SELECT 1 FROM bids b WHERE b.event_id = e.id AND b.withdrawn_at IS NULL)
	`, todayStr, escalateCutoff)
	if err != nil {
		return recruited, 0, err
	}
	var toEscalate []evItem
	for rows2.Next() {
		var it evItem
		if err := rows2.Scan(&it.id, &it.date); err != nil {
			rows2.Close()
			return recruited, 0, err
		}
		toEscalate = append(toEscalate, it)
	}
	rows2.Close()

	for _, ev := range toEscalate {
		_, _ = s.db.ExecContext(ctx, `UPDATE events SET escalated = 1 WHERE id = ?`, ev.id)
		for _, t := range teachers {
			if t.IsAdmin {
				_ = s.hub.SendDM(ctx, transport.Recipient{
					TeacherID: t.ID, Name: t.Name, Phone: t.Phone, Handle: t.Handle,
				}, fmt.Sprintf("⚠️ Admin Escalation: Thursday %s is within %d days and still has ZERO bids. Manage at %s",
					ev.date, resolveDays, s.baseURL))
			}
		}
		escalated++
	}

	return recruited, escalated, nil
}

func (s *Store) nextOpenDates(ctx context.Context, afterDate string, limit int) []string {
	rows, err := s.db.QueryContext(ctx,
		`SELECT date FROM events WHERE status IN ('open','recruiting') AND date > ? ORDER BY date ASC LIMIT ?`,
		afterDate, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err == nil {
			out = append(out, d)
		}
	}
	return out
}

// UpdateEventDetails updates the lesson type, DJ, notes, and help flags.
// Permitted ONLY for the resolved winner or the current tentative holder of that date.
func (s *Store) UpdateEventDetails(ctx context.Context, actor *Teacher, eventID int64, lessonType, lessonDetail, dj, notes string, needLead, needFollow, needCo bool) error {
	if actor == nil {
		return errors.New("authentication required")
	}
	ev, err := s.GetEventView(ctx, eventID, actor.ID, actor.IsAdmin)
	if err != nil {
		return err
	}
	if !ev.CanEditDetails {
		return errors.New("only the instructor holding this date can edit its details")
	}
	lessonType = strings.ToLower(strings.TrimSpace(lessonType))
	if lessonType == "" {
		lessonType = "salsa"
	}
	dj = strings.TrimSpace(dj)
	if strings.EqualFold(dj, "TBD") {
		dj = ""
	}
	b2i := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE events
		SET lesson_type = ?, lesson_detail = ?, dj = ?, notes = ?,
		    need_lead = ?, need_follow = ?, need_co = ?
		WHERE id = ?
	`, lessonType, strings.TrimSpace(lessonDetail), dj, strings.TrimSpace(notes),
		b2i(needLead), b2i(needFollow), b2i(needCo), eventID)
	if err != nil {
		return err
	}

	// If any new help flag was turned on, announce help request in the group per noise policy.
	var needs []string
	if needLead && !ev.NeedLead {
		needs = append(needs, "a Lead")
	}
	if needFollow && !ev.NeedFollow {
		needs = append(needs, "a Follow")
	}
	if needCo && !ev.NeedCo {
		needs = append(needs, "a Co-Instructor")
	}
	if len(needs) > 0 {
		_ = s.hub.PostToGroup(ctx,
			fmt.Sprintf("🤝 %s is looking for %s for Thursday %s (%s). Volunteer here: %s",
				actor.Name, strings.Join(needs, " & "), ev.Date, strings.Title(lessonType), s.baseURL))
	}
	return nil
}

// VolunteerForEvent lets another teacher volunteer as lead, follow, or co-instructor.
// The instructor teaching or bidding on the date cannot volunteer for their own lesson.
func (s *Store) VolunteerForEvent(ctx context.Context, actor *Teacher, eventID int64, role string) error {
	if actor == nil {
		return errors.New("authentication required")
	}
	if role != "lead" && role != "follow" && role != "co" {
		return errors.New("role must be lead, follow, or co")
	}
	ev, err := s.GetEventView(ctx, eventID, actor.ID, actor.IsAdmin)
	if err != nil {
		return err
	}
	if !ev.CanVolunteer {
		return errors.New("you cannot volunteer as a helper on a night you are teaching or bidding on")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO confirmations(teacher_id, event_id, role) VALUES (?, ?, ?)`,
		actor.ID, eventID, role); err != nil {
		return err
	}
	col := "need_lead"
	if role == "follow" {
		col = "need_follow"
	} else if role == "co" {
		col = "need_co"
	}
	_, _ = s.db.ExecContext(ctx, fmt.Sprintf(`UPDATE events SET %s = 0 WHERE id = ?`, col), eventID)
	_ = s.hub.PostToGroup(ctx, fmt.Sprintf("🙌 %s volunteered as %s for Thursday %s!", actor.Name, strings.Title(role), ev.Date))
	return nil
}

// UnvolunteerForEvent removes a volunteer from a role (e.g. if clicked by mistake)
// and re-opens the corresponding need_lead/need_follow/need_co flag on the event.
func (s *Store) UnvolunteerForEvent(ctx context.Context, actor *Teacher, eventID int64, teacherID int64, role string) error {
	if actor == nil {
		return errors.New("authentication required")
	}
	if role != "lead" && role != "follow" && role != "co" {
		return errors.New("role must be lead, follow, or co")
	}
	if teacherID == 0 {
		teacherID = actor.ID
	}
	ev, err := s.GetEventView(ctx, eventID, actor.ID, actor.IsAdmin)
	if err != nil {
		return err
	}
	if teacherID != actor.ID && !ev.CanEditDetails && !actor.IsAdmin {
		return errors.New("you can only remove your own volunteer role")
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM confirmations WHERE event_id = ? AND teacher_id = ? AND role = ?`,
		eventID, teacherID, role)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return errors.New("volunteer role not found")
	}
	col := "need_lead"
	if role == "follow" {
		col = "need_follow"
	} else if role == "co" {
		col = "need_co"
	}
	_, _ = s.db.ExecContext(ctx, fmt.Sprintf(`UPDATE events SET %s = 1 WHERE id = ?`, col), eventID)
	return nil
}

func (s *Store) GetEventView(ctx context.Context, eventID int64, viewerID int64, isAdmin bool) (*EventView, error) {
	list, err := s.queryEvents(ctx, `WHERE e.id = ?`, []any{eventID}, viewerID, isAdmin)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, sql.ErrNoRows
	}
	return &list[0], nil
}

func (s *Store) ListEventsForViewer(ctx context.Context, viewerID int64, isAdmin bool) ([]EventView, error) {
	return s.queryEvents(ctx, `ORDER BY e.date ASC`, nil, viewerID, isAdmin)
}

func (s *Store) queryEvents(ctx context.Context, clause string, args []any, viewerID int64, isAdmin bool) ([]EventView, error) {
	resolveDays := s.GetConfigInt(ctx, "resolve_days_before", 7)
	q := `
		SELECT e.id, e.date, e.status, e.winner_teacher_id, COALESCE(w.name, ''),
		       e.resolved_via, e.lesson_type, e.lesson_detail, e.dj, e.notes,
		       e.need_lead, e.need_follow, e.need_co
		FROM events e
		LEFT JOIN teachers w ON w.id = e.winner_teacher_id
	` + clause
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var out []EventView
	for rows.Next() {
		var ev EventView
		var winID sql.NullInt64
		var nl, nf, nc int
		if err := rows.Scan(&ev.ID, &ev.Date, &ev.Status, &winID, &ev.WinnerName,
			&ev.ResolvedVia, &ev.LessonType, &ev.LessonDetail, &ev.DJ, &ev.Notes,
			&nl, &nf, &nc); err != nil {
			rows.Close()
			return nil, err
		}
		if winID.Valid {
			id := winID.Int64
			ev.WinnerTeacherID = &id
		}
		ev.NeedLead = nl == 1
		ev.NeedFollow = nf == 1
		ev.NeedCo = nc == 1
		if t, err := time.Parse("2006-01-02", ev.Date); err == nil {
			ev.ResolveDeadline = t.AddDate(0, 0, -resolveDays).Format("2006-01-02")
		}
		out = append(out, ev)
	}
	rows.Close()

	for i := range out {
		ev := &out[i]
		// Load volunteers
		vRows, err := s.db.QueryContext(ctx, `
			SELECT c.teacher_id, t.name, c.role
			FROM confirmations c
			JOIN teachers t ON t.id = c.teacher_id
			WHERE c.event_id = ? AND c.role IN ('lead','follow','co')
			ORDER BY c.id ASC
		`, ev.ID)
		if err != nil {
			return nil, err
		}
		for vRows.Next() {
			var v VolunteerView
			if err := vRows.Scan(&v.TeacherID, &v.Name, &v.Role); err == nil {
				ev.Volunteers = append(ev.Volunteers, v)
			}
		}
		vRows.Close()

		// Check viewer's own active bid
		if viewerID > 0 {
			var myKind string
			if err := s.db.QueryRowContext(ctx,
				`SELECT kind FROM bids WHERE event_id = ? AND teacher_id = ? AND withdrawn_at IS NULL`,
				ev.ID, viewerID).Scan(&myKind); err == nil {
				ev.MyBidKind = myKind
			}
		}

		isWinner := ev.WinnerTeacherID != nil && *ev.WinnerTeacherID == viewerID
		evTime, _ := time.Parse("2006-01-02", ev.Date)
		ranked, _ := s.rankActiveBids(ctx, ev.ID, evTime)

		isLeadingBidder := false
		if len(ranked) > 0 && ranked[0].TeacherID == viewerID {
			isLeadingBidder = true
		}

		if ev.Status == "resolved" || ev.Status == "done" {
			ev.CanEditDetails = isWinner
		} else {
			ev.CanEditDetails = isLeadingBidder
		}
		ev.CanVolunteer = viewerID > 0 && !isWinner && ev.MyBidKind == ""

		// Privacy rule: ONLY show bidder names on unresolved dates if help is requested
		// (need_lead / need_follow / need_co) OR if the viewer is Admin.
		if ev.NeedLead || ev.NeedFollow || ev.NeedCo {
			if ev.WinnerName != "" {
				ev.HelpRequester = ev.WinnerName
			} else if len(ranked) > 0 {
				ev.HelpRequester = ranked[0].TeacherName
			}
		}

		if isAdmin {
			for _, rb := range ranked {
				ev.AdminBids = append(ev.AdminBids, rb.BidView)
			}
		}

		if ev.Status == "resolved" || ev.Status == "done" {
			ev.FBEventCopy = FormatFacebookEvent(*ev)
		}
	}

	return out, nil
}

// FormatFacebookEvent creates the semi-automated copy-paste Facebook Event description.
func FormatFacebookEvent(ev EventView) string {
	lessonLabel := strings.Title(ev.LessonType)
	if ev.LessonDetail != "" {
		lessonLabel += " (" + ev.LessonDetail + ")"
	}
	instructor := ev.WinnerName
	if instructor == "" {
		instructor = "TBA"
	}
	var helpers []string
	for _, v := range ev.Volunteers {
		helpers = append(helpers, fmt.Sprintf("%s (%s)", v.Name, v.Role))
	}
	withLine := ""
	if len(helpers) > 0 {
		withLine = "\nAssisting: " + strings.Join(helpers, ", ")
	}
	notesLine := ""
	if ev.Notes != "" {
		notesLine = "\n\n" + ev.Notes
	}
	djLabel := ev.DJ
	if djLabel == "" {
		djLabel = "TBD"
	}
	return fmt.Sprintf(
		"TITLE: Thursday Latin Dancing at Scrumpy's — %s with %s (%s)\n"+
			"DATE: Thursday %s · 7:30 PM – 11:00 PM\n"+
			"LOCATION: Scrumpy's Hard Cider Bar, Fort Collins, CO\n\n"+
			"Join us for Thursday Latin Dancing at Scrumpy's!\n"+
			"🕺 7:30 PM Lesson: %s taught by %s%s\n"+
			"🎧 8:30 PM Social Dancing — Music by DJ %s%s",
		lessonLabel, instructor, ev.Date,
		ev.Date,
		lessonLabel, instructor, withLine,
		djLabel, notesLine,
	)
}

// GenerateWeeklyDigest builds a low-noise WhatsApp group summary of the next 4 weeks.
func (s *Store) GenerateWeeklyDigest(ctx context.Context, now time.Time) (string, error) {
	events, err := s.ListEventsForViewer(ctx, 0, false)
	if err != nil {
		return "", err
	}
	startStr := now.UTC().Format("2006-01-02")
	endStr := now.UTC().AddDate(0, 0, 28).Format("2006-01-02")

	var lines []string
	lines = append(lines, "💃 *Thursday Latin Dancing at Scrumpy's — Next 4 Weeks*")
	for _, ev := range events {
		if ev.Date < startStr || ev.Date > endStr {
			continue
		}
		switch ev.Status {
		case "resolved", "done":
			djLabel := ev.DJ
			if djLabel == "" {
				djLabel = "TBD"
			}
			line := fmt.Sprintf("• *%s*: %s — %s (DJ: %s)", ev.Date, ev.WinnerName, strings.Title(ev.LessonType), djLabel)
			if ev.NeedLead || ev.NeedFollow || ev.NeedCo {
				line += " ⚠️ *[Needs Lead/Follow Help!]*"
			}
			lines = append(lines, line)
		default:
			line := fmt.Sprintf("• *%s*: Open for bids (resolves %s)", ev.Date, ev.ResolveDeadline)
			if ev.NeedLead || ev.NeedFollow || ev.NeedCo {
				req := ev.HelpRequester
				if req == "" {
					req = "A bidder"
				}
				line += fmt.Sprintf(" — 🤝 *%s needs Lead/Follow help*", req)
			}
			lines = append(lines, line)
		}
	}
	lines = append(lines, fmt.Sprintf("\n📅 Bid on dates (6mo open) or volunteer: %s", s.baseURL))
	return strings.Join(lines, "\n"), nil
}

// RequestOTP generates a 6-digit OTP for a registered teacher phone and sends it via Transport.
func (s *Store) RequestOTP(ctx context.Context, phone string, now time.Time) (string, error) {
	t, err := s.GetTeacherByPhone(ctx, phone)
	if err != nil {
		return "", errors.New("phone number not on the active instructor roster — ask admin to add you")
	}
	n, err := rand.Int(rand.Reader, big.NewInt(900000))
	if err != nil {
		return "", err
	}
	code := fmt.Sprintf("%06d", 100000+n.Int64())
	exp := now.UTC().Add(10 * time.Minute).Format(time.RFC3339)

	_, _ = s.db.ExecContext(ctx, `DELETE FROM otp_codes WHERE phone = ?`, t.Phone)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO otp_codes(phone, code, expires_at) VALUES (?, ?, ?)`,
		t.Phone, code, exp); err != nil {
		return "", err
	}
	if err := s.hub.SendOTP(ctx, t.Phone, code); err != nil {
		return "", err
	}
	return code, nil
}

// VerifyOTP checks a 6-digit code and returns a long-lived session token.
func (s *Store) VerifyOTP(ctx context.Context, phone, code string, now time.Time) (string, *Teacher, error) {
	t, err := s.GetTeacherByPhone(ctx, phone)
	if err != nil {
		return "", nil, errors.New("unknown teacher phone")
	}
	var savedCode, expStr string
	err = s.db.QueryRowContext(ctx,
		`SELECT code, expires_at FROM otp_codes WHERE phone = ?`, t.Phone).Scan(&savedCode, &expStr)
	if err != nil {
		return "", nil, errors.New("no active login code found — request a new code")
	}
	expTime, err := time.Parse(time.RFC3339, expStr)
	if err != nil || now.UTC().After(expTime) {
		return "", nil, errors.New("login code expired — request a new code")
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(code)), []byte(savedCode)) != 1 {
		return "", nil, errors.New("invalid 6-digit code")
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM otp_codes WHERE phone = ?`, t.Phone)

	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	tok := hex.EncodeToString(raw)
	sessExp := now.UTC().AddDate(0, 3, 0).Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions(token, teacher_id, expires_at) VALUES (?, ?, ?)`,
		tok, t.ID, sessExp); err != nil {
		return "", nil, err
	}
	return tok, t, nil
}

func (s *Store) GetTeacherBySession(ctx context.Context, token string, now time.Time) (*Teacher, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("empty session")
	}
	var teacherID int64
	var expStr string
	err := s.db.QueryRowContext(ctx,
		`SELECT teacher_id, expires_at FROM sessions WHERE token = ?`, token).Scan(&teacherID, &expStr)
	if err != nil {
		return nil, err
	}
	expTime, err := time.Parse(time.RFC3339, expStr)
	if err != nil || now.UTC().After(expTime) {
		return nil, errors.New("session expired")
	}
	t, err := s.GetTeacherByID(ctx, teacherID, now)
	if err != nil || !t.Active {
		return nil, errors.New("inactive teacher")
	}
	return t, nil
}

// ExecuteBotCommand processes an inbound WhatsApp/Telegram command from a teacher's phone.
func (s *Store) ExecuteBotCommand(ctx context.Context, fromPhone, rawText string, now time.Time) (string, error) {
	t, err := s.GetTeacherByPhone(ctx, fromPhone)
	if err != nil {
		return "", errors.New("your number is not registered on the instructor roster")
	}
	cmd, err := s.hub.ParseCommand(rawText)
	if err != nil {
		return "Commands: `/claim YYYY-MM-DD`, `/token YYYY-MM-DD`, `/lead YYYY-MM-DD`, `/follow YYYY-MM-DD`, `/status`, `/help`. Web app: " + s.baseURL, nil
	}
	switch cmd.Type {
	case transport.CmdHelp:
		return "Commands:\n• `/claim YYYY-MM-DD` — place a normal bid\n• `/token YYYY-MM-DD` — place a Veto Token bid\n• `/lead YYYY-MM-DD` / `/follow YYYY-MM-DD` — volunteer\n• `/status` — your fairness & token balance\nFull calendar & event details: " + s.baseURL, nil
	case transport.CmdStatus:
		return fmt.Sprintf("%s: %d confirmed nights in last 90d | %d/%d Veto Tokens available. Full schedule: %s",
			t.Name, t.FairnessScore, t.TokensAvailable, t.TokensTotal, s.baseURL), nil
	case transport.CmdClaim, transport.CmdToken:
		var evID int64
		if err := s.db.QueryRowContext(ctx, `SELECT id FROM events WHERE date = ?`, cmd.DateArg).Scan(&evID); err != nil {
			return fmt.Sprintf("No Thursday event found on %s. See open dates: %s", cmd.DateArg, s.baseURL), nil
		}
		kind := "normal"
		if cmd.Type == transport.CmdToken {
			kind = "token"
		}
		if err := s.PlaceBid(ctx, t.ID, evID, kind, now); err != nil {
			return "Bid error: " + err.Error(), nil
		}
		return fmt.Sprintf("Bid recorded (%s) for Thursday %s! Set your lesson type & DJ here: %s", kind, cmd.DateArg, s.baseURL), nil
	case transport.CmdLead, transport.CmdFollow:
		var evID int64
		if err := s.db.QueryRowContext(ctx, `SELECT id FROM events WHERE date = ?`, cmd.DateArg).Scan(&evID); err != nil {
			return fmt.Sprintf("No Thursday event found on %s.", cmd.DateArg), nil
		}
		role := "lead"
		if cmd.Type == transport.CmdFollow {
			role = "follow"
		}
		if err := s.VolunteerForEvent(ctx, t, evID, role); err != nil {
			return "Error: " + err.Error(), nil
		}
		return fmt.Sprintf("Thanks %s! You're listed as %s volunteer for %s. Details: %s", t.Name, role, cmd.DateArg, s.baseURL), nil
	default:
		return "See schedule at " + s.baseURL, nil
	}
}
