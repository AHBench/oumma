package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"oumma/internal/repository"
)

// RegisterRoutes mounts the ported AuthController endpoints
// (Java @Path("/api/auth")). These routes are public (no bearer required).
func (s *Service) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/auth/signin", s.handleSignIn)
	mux.HandleFunc("POST /api/auth/register", s.handleRegister)
	mux.HandleFunc("GET /api/auth/activate", s.handleActivate)
	mux.HandleFunc("GET /api/auth/email-available", s.handleEmailAvailable)
	mux.HandleFunc("POST /api/auth/signout", s.handleSignOut)
	mux.HandleFunc("GET /api/auth/session/active", s.handleSessionActive)
	mux.HandleFunc("POST /api/auth/refresh", s.handleRefresh)
	mux.HandleFunc("GET /api/auth/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"message": "Authentication service is healthy - Use standardized /health endpoints for comprehensive checks",
			"status":  "UP",
		})
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeErrCode writes an error with a stable machine-readable code so the
// client can display a translated, human message. `msg` stays as the
// English fallback.
func writeErrCode(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// signInErrorCode maps a Keycloak sign-in failure to a stable code.
func signInErrorCode(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not fully set up"):
		return "account_not_activated"
	case strings.Contains(msg, "disabled"):
		return "account_disabled"
	case strings.Contains(msg, "401"), strings.Contains(msg, "credentials"), strings.Contains(msg, "user_not_found"), strings.Contains(msg, "user disabled"):
		return "invalid_credentials"
	default:
		return "sign_in_failed"
	}
}

func (s *Service) handleSignIn(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	ar, err := s.SignIn(r.Context(), req.Username, req.Password)
	if err != nil {
		s.log.Error("sign in failed", "user", req.Username, "error", err)
		writeErrCode(w, http.StatusUnauthorized, signInErrorCode(err), "Invalid username or password")
		return
	}
	writeJSON(w, http.StatusOK, ar)
}

// handleRegister — public self-registration: creates the user in Keycloak,
// sets its password, confirms the email, and optionally joins the
// `Designer` group when the requester opts in to the designer role.
func (s *Service) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username  string `json:"username"`
		Password  string `json:"password"`
		Email     string `json:"email"`
		FirstName string `json:"firstName"`
		LastName  string `json:"lastName"`
		Designer  bool   `json:"designer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if len(req.Username) < 3 || len(req.Username) > 32 || !validUsername(req.Username) {
		writeErrCode(w, http.StatusBadRequest, "username_invalid", "username must be 3-32 characters (letters, digits, dot, dash, underscore)")
		return
	}
	if len(req.Password) < 6 {
		writeErrCode(w, http.StatusBadRequest, "password_short", "password must be at least 6 characters")
		return
	}
	req.FirstName = strings.TrimSpace(req.FirstName)
	req.LastName = strings.TrimSpace(req.LastName)
	if len(req.FirstName) < 2 || len(req.LastName) < 2 {
		writeErrCode(w, http.StatusBadRequest, "names_required", "first name and last name are required")
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	if req.Email == "" {
		// The Keycloak realm's user profile requires an email; accounts
		// without one cannot sign in ("Account is not fully set up").
		writeErrCode(w, http.StatusBadRequest, "email_required", "email is required")
		return
	}
	if !strings.Contains(req.Email, "@") {
		writeErrCode(w, http.StatusBadRequest, "email_invalid", "a valid email is required")
		return
	}
	ctx := r.Context()
	user, err := s.CreateUser(ctx, CreateUserRequest{
		Username:      req.Username,
		Email:         strings.TrimSpace(req.Email),
		FirstName:     req.FirstName,
		LastName:      req.LastName,
		EmailVerified: boolPtr(false), // verified later via the activation link
	})
	if err != nil {
		s.log.Error("registration failed", "user", req.Username, "error", err)
		msg := err.Error()
		switch {
		case strings.Contains(msg, "same email"):
			writeErrCode(w, http.StatusConflict, "email_taken", "This email address is already associated with another account")
		case strings.Contains(msg, "same username"), strings.Contains(msg, "409"):
			writeErrCode(w, http.StatusConflict, "username_taken", "A user with this username already exists")
		default:
			writeErrCode(w, http.StatusBadGateway, "account_creation_failed", "Failed to create the account: "+msg)
		}
		return
	}
	if err := s.SetUserPassword(ctx, user, req.Password); err != nil {
		writeErrCode(w, http.StatusBadGateway, "password_set_failed", "Account created but password could not be set: "+err.Error())
		return
	}
	joinedDesigner := false
	if req.Designer {
		if err := s.JoinGroup(ctx, user, "Designer"); err != nil {
			s.log.Warn("registration: designer group join failed", "user", req.Username, "error", err)
		}
		joinedDesigner = err == nil
	}
	// Email activation: mint a single-use token and deliver the link. When
	// the user has no email (or no store is wired) there is nothing to
	// activate — the account is ready to use as-is.
	activation := "skipped"
	var activationURL string
	if strings.TrimSpace(req.Email) != "" && s.activations != nil {
		token, terr := newActivationToken()
		if terr == nil {
			now := time.Now().UTC()
			a := repository.EmailActivation{
				Token: token, KeycloakUserID: user, Username: req.Username,
				Email: strings.TrimSpace(req.Email), CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour),
			}
			if serr := s.activations.CreateActivation(ctx, a); serr == nil {
				activationURL = activationLink(token)
				mode, sendErr := s.sendActivationEmail(ctx, req.Email, req.FirstName, req.Username, activationURL)
				if sendErr != nil {
					s.log.Warn("activation email failed", "user", req.Username, "error", sendErr)
					activation = "email_failed"
				} else {
					activation = mode
				}
			} else {
				s.log.Warn("activation token storage failed", "user", req.Username, "error", serr)
				activation = "skipped"
			}
		}
	}
	s.log.Info("user registered", "user", req.Username, "designer", joinedDesigner, "activation", activation)
	resp := map[string]any{
		"registered": true,
		"username":   req.Username,
		"designer":   joinedDesigner,
		"activation": activation,
	}
	if activationURL != "" && (activation == "logged" || activation == "email_failed") {
		// No email gateway (dev) or the send failed — surface the link so the
		// account can still be activated.
		resp["activation_url"] = activationURL
	}
	writeJSON(w, http.StatusCreated, resp)
}

// newActivationToken returns a 32-byte random hex token.
func newActivationToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// activationLink builds the public activation URL (backend-served page).
func activationLink(token string) string {
	base := strings.TrimRight(os.Getenv("ACTIVATION_BASE_URL"), "/")
	if base == "" {
		base = "http://localhost:8095"
	}
	return base + "/api/auth/activate?token=" + token
}

// sendActivationEmail delivers the activation link (SMTP via the Keycloak
// realm settings, then HTTP gateway, then log mode — see SendMail).
func (s *Service) sendActivationEmail(ctx context.Context, to, firstName, username, link string) (string, error) {
	body := fmt.Sprintf(
		"Hello %s,\n\nYour Oumma account (%s) was created. Please activate it by opening the link below (valid for 24 hours):\n\n%s\n\nIf you did not request this account, you can ignore this email.",
		firstName, username, link)
	return s.SendMail(ctx, to, "Activate your Oumma account", body)
}

func validUsername(u string) bool {
	for _, c := range u {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func boolPtr(b bool) *bool { return &b }

func (s *Service) handleSignOut(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.Logout(r.Context(), req.RefreshToken)
	msg := "Successfully signed out"
	if req.RefreshToken == "" {
		msg = "No active session found"
	}
	writeJSON(w, http.StatusOK, SignOutResponse{Message: msg, Success: true})
}

func (s *Service) handleSessionActive(w http.ResponseWriter, r *http.Request) {
	authHeader := r.Header.Get("Authorization")
	status, _ := s.ValidateSession(r.Context(), authHeader)
	// Java always answers 200; inactive sessions carry active=false.
	writeJSON(w, http.StatusOK, status)
}

func (s *Service) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	ar, err := s.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		s.log.Error("token refresh failed", "error", err)
		writeErr(w, http.StatusUnauthorized, "Invalid or expired refresh token")
		return
	}
	writeJSON(w, http.StatusOK, ar)
}

// handleEmailAvailable — public availability check for the registration
// form (debounced while the user types). Returns {available: bool}.
func (s *Service) handleEmailAvailable(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.URL.Query().Get("email"))
	if email == "" || !strings.Contains(email, "@") {
		writeJSON(w, http.StatusOK, map[string]any{"available": true, "checked": false})
		return
	}
	available, err := s.EmailAvailable(r.Context(), email)
	if err != nil {
		s.log.Warn("email availability check failed", "error", err)
		// Fail open: let the register attempt surface the authoritative 409.
		writeJSON(w, http.StatusOK, map[string]any{"available": true, "checked": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": available, "checked": true})
}

// handleActivate serves the email activation link: validates the
// single-use token, confirms the email in Keycloak and renders a small
// result page (the link is opened in a browser).
func (s *Service) handleActivate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	token := strings.TrimSpace(r.URL.Query().Get("token"))
	if token == "" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, activationPage(false, "Invalid activation link — no token provided. Please request a new one."))
		return
	}
	if s.activations == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, activationPage(false, "Activation service is not configured."))
		return
	}
	a, err := s.activations.GetActivation(r.Context(), token)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, activationPage(false, "This activation link is invalid. Please request a new one."))
		return
	}
	if a.UsedAt != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, activationPage(false, "This activation link was already used."))
		return
	}
	if time.Now().UTC().After(a.ExpiresAt) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, activationPage(false, "This activation link has expired (links are valid for 24 hours). Please request a new one."))
		return
	}
	if err := s.ConfirmEmail(r.Context(), a.KeycloakUserID); err != nil {
		s.log.Error("activation confirm-email failed", "user", a.Username, "error", err)
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, activationPage(false, "The account exists but could not be activated right now. Please try again shortly."))
		return
	}
	if err := s.activations.MarkActivationUsed(r.Context(), token); err != nil {
		s.log.Warn("activation mark-used failed", "user", a.Username, "error", err)
	}
	s.log.Info("account activated", "user", a.Username)
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, activationPage(true, "Your Oumma account ("+a.Username+") is now active. You can sign in."))
}

func activationPage(ok bool, message string) string {
	title := "Account activated"
	if !ok {
		title = "Activation problem"
	}
	icon := "✅"
	if !ok {
		icon = "⚠️"
	}
	return `<!doctype html>
<html><head><meta charset="utf-8"><title>Oumma — ` + title + `</title>
<style>
  body{font-family:-apple-system,Segoe UI,Roboto,sans-serif;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0;background:#f5f7fb;color:#1e293b}
  .card{background:#fff;border:1px solid #e2e8f0;border-radius:12px;padding:32px 40px;max-width:480px;text-align:center;box-shadow:0 8px 24px rgba(15,23,42,.08)}
  h1{font-size:20px;margin:12px 0 8px} p{color:#475569;line-height:1.5}
  a.btn{display:inline-block;margin-top:16px;padding:10px 22px;border-radius:8px;background:#2563eb;color:#fff;text-decoration:none;font-weight:600}
</style></head><body>
<div class="card"><div style="font-size:40px">` + icon + `</div><h1>` + title + `</h1><p>` + message + `</p>
<a class="btn" href="/login">Go to sign in</a></div>
</body></html>`
}
