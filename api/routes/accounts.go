package routes

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"elnova-streamkit.local/api/schemas"
	"elnova-streamkit.local/api/store"
)

func (s *Server) limitAuth(c *reqCtx) error {
	if !s.authLimit.Allow(c.ip) {
		return errRateLimited
	}
	return nil
}

func (s *Server) dashboardBase() string {
	if base := s.cfg.ControlPlane.PublicDashboardURL; base != "" {
		return strings.TrimRight(base, "/")
	}
	return "http://localhost:3000"
}

func (s *Server) link(path, token string) string {
	return s.dashboardBase() + path + "?token=" + url.QueryEscape(token)
}

func (s *Server) sendVerification(user *store.User, r *http.Request) {
	token, err := s.store.CreateEmailToken(r.Context(), user.ID, store.PurposeVerifyEmail, verifyTokenTTL)
	if err != nil {
		s.log.Warn("could not create verification token", "error", err)
		return
	}
	body := fmt.Sprintf("Confirm your email address for Elnova StreamKit:\n\n%s\n\nThis link works once and expires in 24 hours.\n", s.link("/verify-email", token))
	if err := s.mailer.Send(user.Email, "Confirm your email address", body); err != nil {
		s.log.Warn("could not send verification email", "error", err)
	}
}

func (s *Server) register(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := s.limitAuth(c); err != nil {
		return err
	}
	var in schemas.Register
	if err := decode(w, r, &in); err != nil {
		return err
	}
	user, org, err := s.store.CreateAccount(r.Context(), in.Email, in.Name, in.Password, in.Organization, s.cfg.RegistrationOpen())
	if errors.Is(err, store.ErrRegistrationClosed) {
		return errorf(http.StatusForbidden, "registration_closed", "This server is not taking new accounts. Ask whoever runs it.")
	}
	if errors.Is(err, store.ErrConflict) {
		return &APIError{Status: http.StatusConflict, Code: "email_taken", Msg: "An account with that email already exists.",
			Fields: schemas.FieldErrors{"email": "is already registered"}}
	}
	if err != nil {
		return err
	}
	c.user = user
	s.audit(r, c, "user.registered", user.Email, nil)
	s.sendVerification(user, r)
	if s.cfg.ControlPlane.RequireEmailVerification {
		writeJSON(w, http.StatusCreated, map[string]any{"user": user, "organization": org, "verification_required": true})
		return nil
	}
	token, err := s.store.CreateSession(r.Context(), user.ID, r.UserAgent(), c.ip)
	if err != nil {
		return err
	}
	s.setSessionCookie(w, token, int(store.SessionTTL.Seconds()))
	writeJSON(w, http.StatusCreated, map[string]any{"user": user, "organization": org})
	return nil
}

func (s *Server) login(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := s.limitAuth(c); err != nil {
		return err
	}
	var in schemas.Login
	if err := decode(w, r, &in); err != nil {
		return err
	}
	user, err := s.store.Authenticate(r.Context(), in.Email, in.Password)
	if errors.Is(err, store.ErrNotFound) {
		// The same message whichever part was wrong, so this cannot be used to discover accounts.
		return errorf(http.StatusUnauthorized, "invalid_credentials", "That email and password do not match.")
	}
	if err != nil {
		return err
	}
	if s.cfg.ControlPlane.RequireEmailVerification && user.EmailVerifiedAt == nil {
		return errorf(http.StatusForbidden, "email_not_verified", "Confirm your email address first. Check your inbox for the link.")
	}
	token, err := s.store.CreateSession(r.Context(), user.ID, r.UserAgent(), c.ip)
	if err != nil {
		return err
	}
	s.setSessionCookie(w, token, int(store.SessionTTL.Seconds()))
	c.user = user
	s.audit(r, c, "user.login", user.Email, nil)
	writeJSON(w, http.StatusOK, map[string]any{"user": user})
	return nil
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := s.store.DeleteSession(r.Context(), c.user.ID, c.sessionID); err != nil {
		return err
	}
	s.setSessionCookie(w, "", -1)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) forgotPassword(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := s.limitAuth(c); err != nil {
		return err
	}
	var in schemas.ForgotPassword
	if err := decode(w, r, &in); err != nil {
		return err
	}
	// The response is the same whether or not the account exists.
	if user, err := s.store.UserByEmail(r.Context(), in.Email); err == nil {
		token, err := s.store.CreateEmailToken(r.Context(), user.ID, store.PurposeResetPassword, resetTokenTTL)
		if err != nil {
			return err
		}
		body := fmt.Sprintf("Someone asked to reset your Elnova StreamKit password. If that was you, open this link:\n\n%s\n\nIt works once and expires in an hour. If it was not you, you can ignore this email.\n", s.link("/reset-password", token))
		if err := s.mailer.Send(user.Email, "Reset your password", body); err != nil {
			s.log.Warn("could not send reset email", "error", err)
		}
		s.audit(r, &reqCtx{user: user, ip: c.ip}, "user.password_reset_requested", user.Email, nil)
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "If that account exists, we sent a reset link."})
	return nil
}

func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := s.limitAuth(c); err != nil {
		return err
	}
	var in schemas.ResetPassword
	if err := decode(w, r, &in); err != nil {
		return err
	}
	userID, err := s.store.ConsumeEmailToken(r.Context(), in.Token, store.PurposeResetPassword)
	if errors.Is(err, store.ErrNotFound) {
		return errorf(http.StatusBadRequest, "invalid_token", "That link has expired or was already used. Request a new one.")
	}
	if err != nil {
		return err
	}
	if err := s.store.SetPassword(r.Context(), userID, in.Password); err != nil {
		return err
	}
	s.audit(r, &reqCtx{user: &store.User{ID: userID}, ip: c.ip}, "user.password_reset", userID, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "Your password was changed. Sign in with the new one."})
	return nil
}

func (s *Server) verifyEmail(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := s.limitAuth(c); err != nil {
		return err
	}
	var in schemas.VerifyEmail
	if err := decode(w, r, &in); err != nil {
		return err
	}
	userID, err := s.store.ConsumeEmailToken(r.Context(), in.Token, store.PurposeVerifyEmail)
	if errors.Is(err, store.ErrNotFound) {
		return errorf(http.StatusBadRequest, "invalid_token", "That link has expired or was already used. Request a new one.")
	}
	if err != nil {
		return err
	}
	if err := s.store.MarkEmailVerified(r.Context(), userID); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "Your email address is confirmed."})
	return nil
}

func (s *Server) resendVerification(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := s.limitAuth(c); err != nil {
		return err
	}
	if c.user.EmailVerifiedAt == nil {
		s.sendVerification(c.user, r)
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "Sent."})
	return nil
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	sessions, err := s.store.ListSessions(r.Context(), c.user.ID, c.sessionID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": sessions})
	return nil
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id := r.PathValue("id")
	if err := s.store.DeleteSession(r.Context(), c.user.ID, id); err != nil {
		return err
	}
	if id == c.sessionID {
		s.setSessionCookie(w, "", -1)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	orgs, err := s.store.OrganizationsOf(r.Context(), c.user.ID)
	if err != nil {
		return err
	}
	projects, err := s.store.ProjectsForUser(r.Context(), c.user.ID)
	if err != nil {
		return err
	}
	if projects == nil {
		projects = []*store.Project{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": c.user, "organizations": orgs, "projects": s.projectViews(projects)})
	return nil
}

// projectView adds what the dashboard needs to show alongside a stored project.
type projectView struct {
	*store.Project
	URL string `json:"url"`
}

func (s *Server) projectViews(projects []*store.Project) []projectView {
	out := make([]projectView, len(projects))
	for i, p := range projects {
		out[i] = projectView{p, s.publicURL()}
	}
	return out
}

func (s *Server) publicURL() string {
	if u := s.cfg.ControlPlane.PublicWSURL; u != "" {
		return u
	}
	return fmt.Sprintf("ws://localhost:%d", s.cfg.Port)
}

// Time window helpers for the analytics routes.
func parseWindow(r *http.Request, def time.Duration) (time.Time, time.Time) {
	to := time.Now()
	from := to.Add(-def)
	if v, err := time.Parse(time.RFC3339, r.URL.Query().Get("from")); err == nil {
		from = v
	}
	if v, err := time.Parse(time.RFC3339, r.URL.Query().Get("to")); err == nil {
		to = v
	}
	return from, to
}
