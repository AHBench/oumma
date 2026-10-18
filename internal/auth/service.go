// Package auth is the Go port of the brief-auth-backend AuthController and
// KeycloakAdminService. It talks to Keycloak directly (password / refresh /
// client_credentials grants, introspection, admin user API) and is exposed
// both externally (HTTP routes on the oumma mux) and internally
// (in-process token validation via Service.ValidateSession).
package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"oumma/internal/repository"
)

// Service holds the Keycloak connection settings.
type Service struct {
	baseURL           string // e.g. http://localhost:8080
	realm             string
	clientID          string
	clientSecret      string
	adminClientID     string
	adminClientSecret string
	log               *slog.Logger
	http              *http.Client
	activations       repository.ActivationStore
}

// SetActivationStore wires the email-activation token store.
func (s *Service) SetActivationStore(st repository.ActivationStore) { s.activations = st }

// Config is the environment-driven service configuration.
type Config struct {
	BaseURL      string
	Realm        string
	ClientID     string
	ClientSecret string
	// AdminClientID/AdminClientSecret select a dedicated Keycloak client for
	// the Admin API (create-user / confirm-email). When empty, the regular
	// client is used (requires it to hold admin scope).
	AdminClientID     string
	AdminClientSecret string
}

func New(cfg Config, log *slog.Logger) *Service {
	return &Service{
		baseURL:           strings.TrimRight(cfg.BaseURL, "/"),
		realm:             cfg.Realm,
		clientID:          cfg.ClientID,
		clientSecret:      cfg.ClientSecret,
		adminClientID:     cfg.AdminClientID,
		adminClientSecret: cfg.AdminClientSecret,
		log:               log,
		http:              &http.Client{Timeout: 10 * time.Second},
	}
}

// AuthResponse mirrors the Java AuthResponse JSON (signin / refresh output).
type AuthResponse struct {
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token"`
	TokenType        string    `json:"token_type"`
	ExpiresIn        *int64    `json:"expires_in"`
	RefreshExpiresIn *int64    `json:"refresh_expires_in"`
	UserInfo         *UserInfo `json:"user_info"`
}

// UserInfo mirrors the Java UserInfo JSON.
type UserInfo struct {
	Department     []string `json:"department"`
	Sub            string   `json:"sub"`
	EmailVerified  *bool    `json:"email_verified"`
	Name           string   `json:"name"`
	PreferredUser  string   `json:"preferred_username"`
	Username       string   `json:"username"`
	GivenName      string   `json:"given_name"`
	FamilyName     string   `json:"family_name"`
	Email          string   `json:"email"`
	Groups         []string `json:"groups"`
}

// SessionStatus mirrors the Java SessionStatusResponse JSON.
type SessionStatus struct {
	Active     bool      `json:"active"`
	UserInfo   *UserInfo `json:"user_info"`
	ExpiresIn  *int64    `json:"expires_in"`
}

// SignOutResponse mirrors the Java SignOutResponse JSON.
type SignOutResponse struct {
	Message string `json:"message"`
	Success bool   `json:"success"`
}

// tokenResponse is the raw Keycloak token endpoint body.
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        *int64 `json:"expires_in"`
	RefreshExpiresIn *int64 `json:"refresh_expires_in"`
}

func (s *Service) tokenEndpoint() string {
	return s.baseURL + "/realms/" + s.realm + "/protocol/openid-connect/token"
}

// requestToken posts a form to the Keycloak token endpoint.
func (s *Service) requestToken(ctx context.Context, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenEndpoint(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("keycloak token endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	return &tr, nil
}

// SignIn performs the Keycloak password grant and enriches the response with
// the claims extracted from the access token (Java signIn behaviour).
func (s *Service) SignIn(ctx context.Context, username, password string) (*AuthResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("client_id", s.clientID)
	form.Set("client_secret", s.clientSecret)
	form.Set("username", username)
	form.Set("password", password)
	tr, err := s.requestToken(ctx, form)
	if err != nil {
		return nil, err
	}
	ar := &AuthResponse{
		AccessToken:      tr.AccessToken,
		RefreshToken:     tr.RefreshToken,
		TokenType:        tr.TokenType,
		ExpiresIn:        tr.ExpiresIn,
		RefreshExpiresIn: tr.RefreshExpiresIn,
		UserInfo:         ExtractUserInfo(tr.AccessToken),
	}
	s.log.Info("sign in successful", "user", username)
	return ar, nil
}

// Refresh performs the Keycloak refresh_token grant.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*AuthResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", s.clientID)
	form.Set("client_secret", s.clientSecret)
	form.Set("refresh_token", refreshToken)
	tr, err := s.requestToken(ctx, form)
	if err != nil {
		return nil, err
	}
	return &AuthResponse{
		AccessToken:      tr.AccessToken,
		RefreshToken:     tr.RefreshToken,
		TokenType:        tr.TokenType,
		ExpiresIn:        tr.ExpiresIn,
		RefreshExpiresIn: tr.RefreshExpiresIn,
		UserInfo:         ExtractUserInfo(tr.AccessToken),
	}, nil
}

// Logout revokes the refresh token (Keycloak logout endpoint). Failures are
// not fatal — Java returns success regardless.
func (s *Service) Logout(ctx context.Context, refreshToken string) {
	if strings.TrimSpace(refreshToken) == "" {
		s.log.Warn("sign out attempted with null or empty refresh token")
		return
	}
	form := url.Values{}
	form.Set("client_id", s.clientID)
	form.Set("client_secret", s.clientSecret)
	form.Set("refresh_token", refreshToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.baseURL+"/realms/"+s.realm+"/protocol/openid-connect/logout", strings.NewReader(form.Encode()))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.http.Do(req)
	if err != nil {
		s.log.Error("keycloak logout failed", "error", err)
		return
	}
	resp.Body.Close()
	s.log.Info("sign out successful")
}

// ValidateSession is the in-process replacement for the former
// GET :8082/api/auth/session/active HTTP call. It introspects the access
// token with Keycloak and returns the enriched claims.
func (s *Service) ValidateSession(ctx context.Context, accessToken string) (*SessionStatus, bool) {
	if strings.TrimSpace(accessToken) == "" {
		return &SessionStatus{Active: false}, false
	}
	accessToken = strings.TrimPrefix(accessToken, "Bearer ")

	form := url.Values{}
	form.Set("client_id", s.clientID)
	form.Set("client_secret", s.clientSecret)
	form.Set("token", accessToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.baseURL+"/realms/"+s.realm+"/protocol/openid-connect/token/introspect", strings.NewReader(form.Encode()))
	if err != nil {
		return &SessionStatus{Active: false}, false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.http.Do(req)
	if err != nil {
		s.log.Error("token introspection failed", "error", err)
		return &SessionStatus{Active: false}, false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var introspected struct {
		Active            bool   `json:"active"`
		Sub               string `json:"sub"`
		Email             string `json:"email"`
		Name              string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
		Exp               *int64 `json:"exp"`
	}
	if err := json.Unmarshal(body, &introspected); err != nil || !introspected.Active {
		return &SessionStatus{Active: false}, false
	}

	ui := ExtractUserInfo(accessToken)
	if ui.Sub == "" {
		ui.Sub = introspected.Sub
	}
	if ui.Email == "" {
		ui.Email = introspected.Email
	}
	if ui.Name == "" {
		ui.Name = introspected.Name
	}
	if ui.PreferredUser == "" {
		ui.PreferredUser = introspected.PreferredUsername
	}
	if ui.Username == "" && ui.PreferredUser != "" {
		ui.Username = ui.PreferredUser
	}

	var expiresIn *int64
	if introspected.Exp != nil {
		remaining := *introspected.Exp - time.Now().Unix()
		if remaining <= 0 {
			return &SessionStatus{Active: false}, false
		}
		expiresIn = &remaining
	}
	return &SessionStatus{Active: true, UserInfo: ui, ExpiresIn: expiresIn}, true
}

// ExtractUserInfo decodes the JWT payload (no signature verification —
// validation happens via introspection) into UserInfo, mirroring the Java
// extractUserInfoFromToken.
func ExtractUserInfo(accessToken string) *UserInfo {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return &UserInfo{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return &UserInfo{}
	}
	var raw struct {
		Sub               string          `json:"sub"`
		Email             string          `json:"email"`
		Name              string          `json:"name"`
		PreferredUsername string          `json:"preferred_username"`
		GivenName         string          `json:"given_name"`
		FamilyName        string          `json:"family_name"`
		EmailVerified     *bool           `json:"email_verified"`
		Department        json.RawMessage `json:"department"`
		Groups            json.RawMessage `json:"groups"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return &UserInfo{}
	}
	ui := &UserInfo{
		Sub:               raw.Sub,
		Email:             raw.Email,
		Name:              raw.Name,
		PreferredUser:     raw.PreferredUsername,
		Username:          raw.PreferredUsername,
		GivenName:         raw.GivenName,
		FamilyName:        raw.FamilyName,
		EmailVerified:     raw.EmailVerified,
	}
	var dept []string
	if err := json.Unmarshal(raw.Department, &dept); err == nil {
		ui.Department = dept
	}
	var groups []string
	if err := json.Unmarshal(raw.Groups, &groups); err == nil {
		// Keycloak prefixes group paths with "/" (e.g. "/Managers"). Strip it
		// so the app and the engine compare plain group names.
		norm := make([]string, 0, len(groups))
		for _, g := range groups {
			g = strings.TrimPrefix(g, "/")
			if g != "" {
				norm = append(norm, g)
			}
		}
		ui.Groups = norm
	}
	return ui
}

// ---------------------------------------------------------------------------
// Keycloak Admin API (create-user / confirm-email)
// ---------------------------------------------------------------------------

// adminToken fetches a client_credentials token for the Admin API (dedicated
// admin client when configured, otherwise the regular client).
func (s *Service) adminToken(ctx context.Context) (string, error) {
	clientID, secret := s.clientID, s.clientSecret
	if s.adminClientID != "" {
		clientID, secret = s.adminClientID, s.adminClientSecret
	}
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", clientID)
	form.Set("client_secret", secret)
	tr, err := s.requestToken(ctx, form)
	if err != nil {
		return "", fmt.Errorf("failed to authenticate with Keycloak Admin API: %w", err)
	}
	return tr.AccessToken, nil
}

// CreateUser creates a user in the realm via the Keycloak Admin API and
// returns the new user's id (from the Location header).
func (s *Service) CreateUser(ctx context.Context, req CreateUserRequest) (string, error) {
	token, err := s.adminToken(ctx)
	if err != nil {
		return "", err
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	verified := false
	if req.EmailVerified != nil {
		verified = *req.EmailVerified
	}
	body, _ := json.Marshal(map[string]any{
		"username":      req.Username,
		"email":         req.Email,
		"firstName":     req.FirstName,
		"lastName":      req.LastName,
		"enabled":       enabled,
		"emailVerified": verified,
	})
	s.log.Info("creating user in Keycloak", "user", req.Username)
	resp, err := s.adminRequest(ctx, http.MethodPost, "/admin/realms/"+s.realm+"/users", token, body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("keycloak returned %d for user creation: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	loc := resp.Header.Get("Location")
	if i := strings.LastIndex(loc, "/"); i >= 0 && i+1 < len(loc) {
		return loc[i+1:], nil
	}
	return "", fmt.Errorf("keycloak created the user but returned no Location header")
}

// SetUserPassword sets the user's password credential.
func (s *Service) SetUserPassword(ctx context.Context, userID, password string) error {
	token, err := s.adminToken(ctx)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{
		"type":      "password",
		"value":     password,
		"temporary": false,
	})
	s.log.Info("setting user password in Keycloak", "user_id", userID)
	resp, err := s.adminRequest(ctx, http.MethodPut, "/admin/realms/"+s.realm+"/users/"+url.PathEscape(userID)+"/reset-password", token, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("keycloak returned %d for password setting", resp.StatusCode)
	}
	return nil
}

// JoinGroup makes the user a member of a realm group, creating the group
// (top-level) when it does not exist yet.
func (s *Service) JoinGroup(ctx context.Context, userID, groupName string) error {
	token, err := s.adminToken(ctx)
	if err != nil {
		return err
	}
	groupID, _, err := s.resolveGroup(ctx, token, groupName)
	if err != nil {
		return err
	}
	return s.putGroupMembership(ctx, token, userID, groupID)
}

// SyncGroup ensures the realm group exists and reports whether it was
// already there ("exists") or just created ("created").
func (s *Service) SyncGroup(ctx context.Context, groupName string) (string, error) {
	token, err := s.adminToken(ctx)
	if err != nil {
		return "", err
	}
	_, existed, err := s.resolveGroup(ctx, token, groupName)
	if err != nil {
		return "", err
	}
	if existed {
		return "exists", nil
	}
	return "created", nil
}

// UserRef is a Keycloak user in the realm (for the designer's user
// assignment UI).
type UserRef struct {
	ID        string   `json:"id"`
	Username  string   `json:"username"`
	Email     string   `json:"email"`
	FirstName string   `json:"firstName"`
	LastName  string   `json:"lastName"`
	Groups    []string `json:"groups"`
}

// ListUsers returns every user in the realm (full representation, which
// carries their group names).
func (s *Service) ListUsers(ctx context.Context) ([]UserRef, error) {
	token, err := s.adminToken(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := s.adminGet(ctx, "/admin/realms/"+s.realm+"/users?max=1000", token)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("keycloak returned %d for user listing", resp.StatusCode)
	}
	var raw []struct {
		ID        string   `json:"id"`
		Username  string   `json:"username"`
		Email     string   `json:"email"`
		FirstName string   `json:"firstName"`
		LastName  string   `json:"lastName"`
		Groups    []struct {
			Name string `json:"name"`
		} `json:"groups"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]UserRef, 0, len(raw))
	for _, u := range raw {
		groups := make([]string, 0, len(u.Groups))
		for _, g := range u.Groups {
			groups = append(groups, strings.TrimPrefix(g.Name, "/"))
		}
		out = append(out, UserRef{
			ID: u.ID, Username: u.Username, Email: u.Email,
			FirstName: u.FirstName, LastName: u.LastName, Groups: groups,
		})
	}
	return out, nil
}

// EmailAvailable reports whether no realm user has the given email
// (Keycloak enforces unique emails per realm). Used by the registration
// form to warn the user live while they type.
func (s *Service) EmailAvailable(ctx context.Context, email string) (bool, error) {
	token, err := s.adminToken(ctx)
	if err != nil {
		return false, err
	}
	q := url.Values{}
	q.Set("email", email)
	q.Set("exact", "true")
	resp, err := s.adminGet(ctx, "/admin/realms/"+s.realm+"/users?"+q.Encode(), token)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("keycloak returned %d for email search", resp.StatusCode)
	}
	var users []struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return false, err
	}
	for _, u := range users {
		if strings.EqualFold(u.Email, email) {
			return false, nil
		}
	}
	return true, nil
}

// FindUserIDByUsername resolves a Keycloak user id from the username.
func (s *Service) FindUserIDByUsername(ctx context.Context, username string) (string, error) {
	token, err := s.adminToken(ctx)
	if err != nil {
		return "", err
	}
	q := url.Values{}
	q.Set("username", username)
	q.Set("exact", "true")
	resp, err := s.adminGet(ctx, "/admin/realms/"+s.realm+"/users?"+q.Encode(), token)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("keycloak returned %d for user lookup", resp.StatusCode)
	}
	var users []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		return "", err
	}
	if len(users) == 0 {
		return "", fmt.Errorf("user not found in Keycloak: %s", username)
	}
	return users[0].ID, nil
}

// ListGroupMembers returns the members of a realm group (by group id).
func (s *Service) ListGroupMembers(ctx context.Context, groupID string) ([]UserRef, error) {
	token, err := s.adminToken(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := s.adminGet(ctx, "/admin/realms/"+s.realm+"/groups/"+url.PathEscape(groupID)+"/members?max=200", token)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("keycloak returned %d for group members", resp.StatusCode)
	}
	var raw []struct {
		ID        string `json:"id"`
		Username  string `json:"username"`
		Email     string `json:"email"`
		FirstName string `json:"firstName"`
		LastName  string `json:"lastName"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]UserRef, 0, len(raw))
	for _, u := range raw {
		out = append(out, UserRef{ID: u.ID, Username: u.Username, Email: u.Email, FirstName: u.FirstName, LastName: u.LastName})
	}
	return out, nil
}

// ResolveGroupID returns the id of a realm group (creating it when
// missing) — used to look up role groups from their names.
func (s *Service) ResolveGroupID(ctx context.Context, groupName string) (string, error) {
	token, err := s.adminToken(ctx)
	if err != nil {
		return "", err
	}
	id, _, err := s.resolveGroup(ctx, token, groupName)
	return id, err
}

// resolveGroup finds a top-level realm group by exact name, creating it
// when missing. Returns (groupID, alreadyExisted, error).
func (s *Service) resolveGroup(ctx context.Context, token, groupName string) (string, bool, error) {
	q := url.Values{}
	q.Set("exact", "true")
	q.Set("name", groupName)
	listResp, err := s.adminGet(ctx, "/admin/realms/"+s.realm+"/groups?"+q.Encode(), token)
	if err != nil {
		return "", false, err
	}
	var groups []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	_ = json.NewDecoder(listResp.Body).Decode(&groups)
	listResp.Body.Close()
	for _, g := range groups {
		if strings.EqualFold(g.Name, groupName) {
			return g.ID, true, nil
		}
	}
	// create the group
	body, _ := json.Marshal(map[string]any{"name": groupName})
	cResp, cerr := s.adminRequest(ctx, http.MethodPost, "/admin/realms/"+s.realm+"/groups", token, body)
	if cerr != nil {
		return "", false, cerr
	}
	loc := cResp.Header.Get("Location")
	io.Copy(io.Discard, cResp.Body)
	cResp.Body.Close()
	if cResp.StatusCode != http.StatusCreated {
		return "", false, fmt.Errorf("keycloak returned %d for group creation", cResp.StatusCode)
	}
	if i := strings.LastIndex(loc, "/"); i >= 0 && i+1 < len(loc) {
		return loc[i+1:], false, nil
	}
	return "", false, fmt.Errorf("keycloak created group %q but returned no Location header", groupName)
}

func (s *Service) putGroupMembership(ctx context.Context, token, userID, groupID string) error {
	resp, err := s.adminRequest(ctx, http.MethodPut, "/admin/realms/"+s.realm+"/users/"+url.PathEscape(userID)+"/groups/"+url.PathEscape(groupID), token, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("keycloak returned %d for group join", resp.StatusCode)
	}
	return nil
}

// adminGet is a GET variant of adminRequest (no body).
func (s *Service) adminGet(ctx context.Context, path, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return s.http.Do(req)
}

// ConfirmEmail sets emailVerified=true on the given user.
func (s *Service) ConfirmEmail(ctx context.Context, userID string) error {
	token, err := s.adminToken(ctx)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"emailVerified": true})
	s.log.Info("confirming email", "user_id", userID)
	resp, err := s.adminRequest(ctx, http.MethodPut, "/admin/realms/"+s.realm+"/users/"+url.PathEscape(userID), token, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("keycloak returned %d for email confirmation", resp.StatusCode)
	}
	return nil
}

// CreateUserRequest mirrors the Java UserCreateRequestDTO.
type CreateUserRequest struct {
	Username      string `json:"username"`
	Email         string `json:"email"`
	FirstName     string `json:"firstName"`
	LastName      string `json:"lastName"`
	Enabled       *bool  `json:"enabled"`
	EmailVerified *bool  `json:"emailVerified"`
}

func (s *Service) adminRequest(ctx context.Context, method, path, token string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	return s.http.Do(req)
}
