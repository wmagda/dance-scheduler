// Package server provides the HTTP API and embedded PWA frontend for the
// Thursday Latin Dancing instructor scheduler.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wojtek/dance-scheduler/internal/store"
)

//go:embed static/*
var staticFS embed.FS

const sessionCookieName = "dance_session"

// OTP endpoint rate limits (sliding window, per process).
const (
	otpWindow             = 15 * time.Minute
	otpRequestMaxPerPhone = 3
	otpRequestMaxPerIP    = 15
	otpRequestIPWindow    = time.Hour
	otpVerifyMaxPerPhone  = 10
)

type Server struct {
	store *store.Store
	mux   *http.ServeMux

	rateMu sync.Mutex
	otpReq map[string][]time.Time // request-otp hits, keyed "target:.." / "ip:.."
	otpVer map[string][]time.Time // verify-otp attempts, keyed "target:.."
}

func New(st *store.Store) *Server {
	s := &Server{
		store:  st,
		mux:    http.NewServeMux(),
		otpReq: make(map[string][]time.Time),
		otpVer: make(map[string][]time.Time),
	}
	s.routes()
	return s
}

// ServeHTTP is the public perimeter: the bot webhook is localhost-only (and
// rejected if forwarded through Cloudflare Tunnel), and every /api route
// except the login endpoints requires a session. Static assets stay public so
// the login/signup page renders for unauthenticated visitors.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/bot/inbound" {
		if !isLocalRequest(r, ipIsLoopback) {
			writeErr(w, http.StatusForbidden, "forbidden")
			return
		}
		s.mux.ServeHTTP(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") &&
		r.URL.Path != "/api/auth/request-otp" &&
		r.URL.Path != "/api/auth/verify-otp" &&
		r.URL.Path != "/api/auth/logout" {
		if s.currentTeacher(r) == nil {
			writeErr(w, http.StatusUnauthorized, "login required")
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

type ipPredicate func(net.IP) bool

func ipIsLoopback(ip net.IP) bool { return ip.IsLoopback() }

// isCloudflareProxied detects traffic forwarded by cloudflared from the public
// internet (which arrives at the origin over 127.0.0.1 carrying CF-Connecting-IP
// and Cf-Ray headers). Such requests must never be treated as local/LAN.
func isCloudflareProxied(r *http.Request) bool {
	return strings.TrimSpace(r.Header.Get("CF-Connecting-IP")) != "" ||
		strings.TrimSpace(r.Header.Get("Cf-Ray")) != ""
}

// isPrivateRequest reports whether the request originates directly from the
// local machine or LAN (loopback or RFC1918, NOT via Cloudflare Tunnel) — used
// to keep manual-mode OTP codes off the public internet.
func isPrivateRequest(r *http.Request) bool {
	if isCloudflareProxied(r) {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

func isLocalRequest(r *http.Request, pred ipPredicate) bool {
	if isCloudflareProxied(r) {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && pred(ip)
}

// allow records a hit for key and reports whether it stays within max over
// the sliding window.
func (s *Server) allow(hits map[string][]time.Time, key string, max int, window time.Duration) bool {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	now := time.Now()
	kept := hits[key][:0]
	for _, t := range hits[key] {
		if now.Sub(t) < window {
			kept = append(kept, t)
		}
	}
	if len(kept) >= max {
		hits[key] = kept
		return false
	}
	hits[key] = append(kept, now)
	return true
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/state", s.handleState)
	s.mux.HandleFunc("POST /api/auth/request-otp", s.handleRequestOTP)
	s.mux.HandleFunc("POST /api/auth/verify-otp", s.handleVerifyOTP)
	s.mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	s.mux.HandleFunc("POST /api/profile", s.handleUpdateProfile)

	s.mux.HandleFunc("POST /api/bids", s.handlePlaceBid)
	s.mux.HandleFunc("POST /api/bids/withdraw", s.handleWithdrawBid)
	s.mux.HandleFunc("POST /api/events/details", s.handleUpdateEventDetails)
	s.mux.HandleFunc("POST /api/events/volunteer", s.handleVolunteer)
	s.mux.HandleFunc("POST /api/events/unvolunteer", s.handleUnvolunteer)

	s.mux.HandleFunc("POST /api/bot/inbound", s.handleBotInbound)

	s.mux.HandleFunc("POST /api/admin/teachers", s.handleAdminAddTeacher)
	s.mux.HandleFunc("POST /api/admin/teachers/remove", s.handleAdminRemoveTeacher)
	s.mux.HandleFunc("POST /api/admin/tokens/grant", s.handleAdminGrantToken)
	s.mux.HandleFunc("POST /api/admin/config", s.handleAdminUpdateConfig)
	s.mux.HandleFunc("POST /api/admin/resolve", s.handleAdminResolve)
	s.mux.HandleFunc("POST /api/admin/events/skip", s.handleAdminSkipEvent)
	s.mux.HandleFunc("GET /api/admin/invite", s.handleAdminGetInvite)
	s.mux.HandleFunc("POST /api/admin/invite/rotate", s.handleAdminRotateInvite)
	s.mux.HandleFunc("POST /api/admin/invite/revoke", s.handleAdminRevokeInvite)

	sub, _ := fs.Sub(staticFS, "static")
	s.mux.Handle("/", http.FileServer(http.FS(sub)))
}

func (s *Server) currentTeacher(r *http.Request) *store.Teacher {
	tok := ""
	if c, err := r.Cookie(sessionCookieName); err == nil {
		tok = c.Value
	}
	if tok == "" {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			tok = strings.TrimPrefix(auth, "Bearer ")
		}
	}
	if tok == "" {
		return nil
	}
	t, err := s.store.GetTeacherBySession(r.Context(), tok, time.Now().UTC())
	if err != nil {
		return nil
	}
	return t
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now().UTC()
	viewer := s.currentTeacher(r)
	var viewerID int64
	isAdmin := false
	if viewer != nil {
		viewerID = viewer.ID
		isAdmin = viewer.IsAdmin
	}

	events, err := s.store.ListUpcomingEventsForViewer(ctx, viewerID, isAdmin, now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	teachers, err := s.store.ListTeachers(ctx, now)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	tokensUsed, err := s.store.ListUsedTokens(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	cfg, err := s.store.GetConfigMap(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	digest, _ := s.store.GenerateWeeklyDigest(ctx, now)

	var outbox []store.OutboxEntry
	var inviteStatus *store.InviteStatus
	inviteToken := ""
	if isAdmin {
		outbox, _ = s.store.ListOutbox(ctx, 50)
		if st, err := s.store.GetInviteStatus(ctx, now); err == nil {
			inviteStatus = &st
			if st.Active {
				inviteToken = st.Token
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"viewer":         viewer,
		"events":         events,
		"teachers":       teachers,
		"tokens_used":    tokensUsed,
		"config":         cfg,
		"transport_mode": s.store.Hub().Name(),
		"digest":         digest,
		"outbox":         outbox,
		"invite":         inviteStatus,
		"invite_token":   inviteToken,
	})
}

func normalizeTargetKey(email, phone string) string {
	if strings.TrimSpace(email) != "" {
		return store.NormalizeEmail(email)
	}
	p := store.NormalizePhone(phone)
	if p != "" {
		return p
	}
	return strings.ToLower(strings.TrimSpace(phone))
}

func (s *Server) handleRequestOTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email  string `json:"email"`
		Phone  string `json:"phone"`
		Name   string `json:"name,omitempty"`
		Invite string `json:"invite,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	contact := strings.TrimSpace(req.Email)
	if contact == "" {
		contact = strings.TrimSpace(req.Phone)
	}
	targetKey := normalizeTargetKey(req.Email, req.Phone)
	if !s.allow(s.otpReq, "target:"+targetKey, otpRequestMaxPerPhone, otpWindow) {
		writeErr(w, http.StatusTooManyRequests, "too many code requests — try again in a few minutes")
		return
	}
	ipKey := "ip:" + clientIP(r)
	if !s.allow(s.otpReq, ipKey, otpRequestMaxPerIP, otpRequestIPWindow) {
		writeErr(w, http.StatusTooManyRequests, "too many code requests from this network — try again later")
		return
	}

	now := time.Now().UTC()
	var code string
	var err error

	if _, lookupErr := s.store.GetTeacherByContact(r.Context(), contact); lookupErr == nil {
		code, err = s.store.RequestOTP(r.Context(), contact, now)
	} else {
		// Unregistered caller: permit self-signup ONLY with an active, non-expired
		// invite token, and defer creating the roster row until VerifyOTP proves
		// ownership of the email address.
		signupEmail := req.Email
		if signupEmail == "" && strings.Contains(req.Phone, "@") {
			signupEmail = req.Phone
		}
		if strings.TrimSpace(signupEmail) == "" {
			if !s.store.ValidateInviteToken(r.Context(), req.Invite, now) {
				writeErr(w, http.StatusForbidden, "not on the instructor roster — use the invite link from the WhatsApp group, or ask an admin to add you")
				return
			}
			writeErr(w, http.StatusBadRequest, "email address is required to sign up")
			return
		}
		code, err = s.store.RequestSignupOTP(r.Context(), signupEmail, req.Name, req.Phone, req.Invite, now)
		if errors.Is(err, store.ErrInvalidInvite) {
			writeErr(w, http.StatusForbidden, err.Error())
			return
		}
	}

	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	resp := map[string]any{
		"ok":             true,
		"transport_mode": s.store.Hub().Name(),
	}
	if s.store.Hub().Name() == "manual" {
		if isPrivateRequest(r) {
			resp["manual_mode_code"] = code
		} else {
			log.Printf("otp: manual-mode code for %s suppressed from response (non-local request), code=%s", contact, code)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if cfIP := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cfIP != "" {
			return cfIP
		}
	}
	return host
}

func (s *Server) handleVerifyOTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
		Phone string `json:"phone"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	contact := strings.TrimSpace(req.Email)
	if contact == "" {
		contact = strings.TrimSpace(req.Phone)
	}
	targetKey := normalizeTargetKey(req.Email, req.Phone)
	if !s.allow(s.otpVer, "target:"+targetKey, otpVerifyMaxPerPhone, otpWindow) {
		writeErr(w, http.StatusTooManyRequests, "too many attempts — request a new code in a few minutes")
		return
	}
	tok, teacher, err := s.store.VerifyOTP(r.Context(), contact, req.Code, time.Now().UTC())
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   90 * 24 * 3600,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"token":   tok,
		"teacher": teacher,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	viewer := s.currentTeacher(r)
	if viewer == nil {
		writeErr(w, http.StatusUnauthorized, "login required")
		return
	}
	var req struct {
		Name  string `json:"name"`
		Email string `json:"email"`
		Phone string `json:"phone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = viewer.Name
	}
	t, err := s.store.UpdateTeacherProfile(r.Context(), viewer.ID, name, req.Email, req.Phone, time.Now().UTC().Year())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handlePlaceBid(w http.ResponseWriter, r *http.Request) {
	viewer := s.currentTeacher(r)
	if viewer == nil {
		writeErr(w, http.StatusUnauthorized, "please log in first")
		return
	}
	var req struct {
		EventID int64  `json:"event_id"`
		Kind    string `json:"kind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Kind == "" {
		req.Kind = "normal"
	}
	if err := s.store.PlaceBid(r.Context(), viewer.ID, req.EventID, req.Kind, time.Now().UTC()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleWithdrawBid(w http.ResponseWriter, r *http.Request) {
	viewer := s.currentTeacher(r)
	if viewer == nil {
		writeErr(w, http.StatusUnauthorized, "please log in first")
		return
	}
	var req struct {
		EventID int64 `json:"event_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.store.WithdrawBid(r.Context(), viewer.ID, req.EventID, time.Now().UTC()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleUpdateEventDetails(w http.ResponseWriter, r *http.Request) {
	viewer := s.currentTeacher(r)
	if viewer == nil {
		writeErr(w, http.StatusUnauthorized, "please log in first")
		return
	}
	var req struct {
		EventID      int64  `json:"event_id"`
		LessonType   string `json:"lesson_type"`
		LessonDetail string `json:"lesson_detail"`
		DJ           string `json:"dj"`
		Notes        string `json:"notes"`
		NeedLead     bool   `json:"need_lead"`
		NeedFollow   bool   `json:"need_follow"`
		NeedCo       bool   `json:"need_co"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.store.UpdateEventDetails(r.Context(), viewer, req.EventID,
		req.LessonType, req.LessonDetail, req.DJ, req.Notes,
		req.NeedLead, req.NeedFollow, req.NeedCo); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleVolunteer(w http.ResponseWriter, r *http.Request) {
	viewer := s.currentTeacher(r)
	if viewer == nil {
		writeErr(w, http.StatusUnauthorized, "please log in first")
		return
	}
	var req struct {
		EventID int64  `json:"event_id"`
		Role    string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.store.VolunteerForEvent(r.Context(), viewer, req.EventID, req.Role); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleUnvolunteer(w http.ResponseWriter, r *http.Request) {
	viewer := s.currentTeacher(r)
	if viewer == nil {
		writeErr(w, http.StatusUnauthorized, "please log in first")
		return
	}
	var req struct {
		EventID   int64  `json:"event_id"`
		TeacherID int64  `json:"teacher_id"`
		Role      string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.store.UnvolunteerForEvent(r.Context(), viewer, req.EventID, req.TeacherID, req.Role); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleBotInbound(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone string `json:"phone"`
		Text  string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	reply, err := s.store.ExecuteBotCommand(r.Context(), req.Phone, req.Text, time.Now().UTC())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"reply": reply})
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) *store.Teacher {
	v := s.currentTeacher(r)
	if v == nil || !v.IsAdmin {
		writeErr(w, http.StatusForbidden, "admin access required")
		return nil
	}
	return v
}

func (s *Server) handleAdminAddTeacher(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	var req struct {
		Name    string `json:"name"`
		Email   string `json:"email"`
		Phone   string `json:"phone"`
		Handle  string `json:"handle"`
		IsAdmin bool   `json:"is_admin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	t, err := s.store.SaveTeacher(r.Context(), req.Name, req.Email, req.Phone, req.Handle, req.IsAdmin, time.Now().UTC().Year())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleAdminRemoveTeacher(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	var req struct {
		TeacherID int64 `json:"teacher_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.store.RemoveTeacher(r.Context(), req.TeacherID); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAdminGrantToken(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	var req struct {
		TeacherID int64  `json:"teacher_id"`
		Year      int    `json:"year"`
		Reason    string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.store.GrantExtraToken(r.Context(), req.TeacherID, req.Year, req.Reason); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAdminUpdateConfig(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	var updates map[string]string
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.store.UpdateConfig(r.Context(), updates); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = s.store.EnsureUpcomingThursdays(r.Context(), time.Now().UTC())
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAdminGetInvite(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	st, err := s.store.GetInviteStatus(r.Context(), time.Now().UTC())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	tok := ""
	if st.Active {
		tok = st.Token
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"invite_token": tok,
		"expires_at":   st.ExpiresAt,
		"active":       st.Active,
	})
}

func (s *Server) handleAdminRotateInvite(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	var req struct {
		TTLDays int `json:"ttl_days"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	st, err := s.store.RotateInviteToken(r.Context(), time.Now().UTC(), req.TTLDays)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"invite_token": st.Token,
		"expires_at":   st.ExpiresAt,
		"active":       st.Active,
	})
}

func (s *Server) handleAdminRevokeInvite(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	if err := s.store.RevokeInviteToken(r.Context()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"invite_token": "",
		"expires_at":   "",
		"active":       false,
	})
}

func (s *Server) handleAdminResolve(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	var req struct {
		EventID int64 `json:"event_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	now := time.Now().UTC()
	resolved, err := s.store.RunResolver(context.Background(), now, req.EventID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	recruited, escalated := 0, 0
	if req.EventID == 0 {
		recruited, escalated, _ = s.store.RunRecruitment(context.Background(), now)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resolved":  resolved,
		"recruited": recruited,
		"escalated": escalated,
	})
}

func (s *Server) handleAdminSkipEvent(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	var req struct {
		EventID int64  `json:"event_id"`
		Skip    bool   `json:"skip"`
		Reason  string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.store.SetEventSkipped(r.Context(), req.EventID, req.Skip, req.Reason, time.Now().UTC()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
