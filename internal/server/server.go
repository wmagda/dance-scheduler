// Package server provides the HTTP API and embedded PWA frontend for the
// Thursday Latin Dancing instructor scheduler.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/wojtek/dance-scheduler/internal/store"
)

//go:embed static/*
var staticFS embed.FS

const sessionCookieName = "dance_session"

type Server struct {
	store *store.Store
	mux   *http.ServeMux
}

func New(st *store.Store) *Server {
	s := &Server{
		store: st,
		mux:   http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/state", s.handleState)
	s.mux.HandleFunc("POST /api/auth/request-otp", s.handleRequestOTP)
	s.mux.HandleFunc("POST /api/auth/verify-otp", s.handleVerifyOTP)
	s.mux.HandleFunc("POST /api/auth/logout", s.handleLogout)

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

	events, err := s.store.ListEventsForViewer(ctx, viewerID, isAdmin)
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
	if isAdmin {
		outbox, _ = s.store.ListOutbox(ctx, 50)
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
	})
}

func (s *Server) handleRequestOTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone string `json:"phone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	code, err := s.store.RequestOTP(r.Context(), req.Phone, time.Now().UTC())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	resp := map[string]any{
		"ok":             true,
		"transport_mode": s.store.Hub().Name(),
	}
	// In manual mode (before the WhatsApp bot is linked), surface the OTP code
	// directly in the response so local/LAN onboarding works out of the box.
	if s.store.Hub().Name() == "manual" {
		resp["manual_mode_code"] = code
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleVerifyOTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone string `json:"phone"`
		Code  string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	tok, teacher, err := s.store.VerifyOTP(r.Context(), req.Phone, req.Code, time.Now().UTC())
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

func (s *Server) handlePlaceBid(w http.ResponseWriter, r *http.Request) {
	viewer := s.currentTeacher(r)
	if viewer == nil {
		writeErr(w, http.StatusUnauthorized, "please log in with your phone number first")
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
		Phone   string `json:"phone"`
		Handle  string `json:"handle"`
		IsAdmin bool   `json:"is_admin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	t, err := s.store.CreateTeacher(r.Context(), req.Name, req.Phone, req.Handle, req.IsAdmin, time.Now().UTC().Year())
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
