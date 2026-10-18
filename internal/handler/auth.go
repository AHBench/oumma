package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"oumma/internal/domain"
	"oumma/internal/engine"
)

// AuthSessionResponse mirrors the Java AuthSessionResponse POJO.
// The auth server must return this JSON body on a successful GET to the
// session validation endpoint.
type AuthSessionResponse struct {
	Active   bool     `json:"active"`
	Username string   `json:"preferred_username"`
	Groups   []string `json:"groups"`
	// Departments holds the caller's allowed department codes extracted from
	// the JWT claim "department" (a flat string array: ["1002"]).
	Departments []string `json:"-"`
}

// AuthClaims is the subset stored in the request context after a successful
// token validation. It is what downstream handlers read via claimsFromContext.
type AuthClaims = AuthSessionResponse

// CORS returns a middleware that adds CORS headers to every response and
// handles pre-flight OPTIONS requests before passing to the next handler.
func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always set a permissive origin header for dev; consider restricting in prod.
		w.Header().Set("Access-Control-Allow-Origin", "*")

		// Always advertise allowed methods.
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")

		// Base allowed headers (what the backend explicitly supports).
		baseAllowed := "Authorization, Content-Type, Accept"

		// If the browser sent an Access-Control-Request-Headers list, echo it
		// so non-simple headers are accepted without requiring manual server edits.
		reqHdrs := r.Header.Get("Access-Control-Request-Headers")
		allowHeaders := baseAllowed
		if reqHdrs != "" {
			allowHeaders = baseAllowed + ", " + reqHdrs
		}
		w.Header().Set("Access-Control-Allow-Headers", allowHeaders)

		// Cache preflight responses for 10 minutes.
		w.Header().Set("Access-Control-Max-Age", "600")

		// Diagnostic logging: if the request asked for headers not present in the
		// backend's base allowed list, log which ones are missing so operator can
		// harden configuration if desired.
		if reqHdrs != "" {
			// Build a map of baseAllowed entries for case-insensitive comparison.
			baseMap := map[string]struct{}{}
			for _, h := range strings.Split(baseAllowed, ",") {
				baseMap[strings.ToLower(strings.TrimSpace(h))] = struct{}{}
			}
			// Check each requested header
			for _, h := range strings.Split(reqHdrs, ",") {
				hh := strings.ToLower(strings.TrimSpace(h))
				if _, ok := baseMap[hh]; !ok {
					slog.Default().Warn("preflight requesting header not in base allow-list", "header", hh)
				}
			}
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

type contextKey string

const claimsContextKey contextKey = "auth_claims"

// joinURL concatenates a base URL and a path segment, avoiding duplicate
// slashes — matching the Java BearerTokenAuthFilter URL-join logic.
func joinURL(base, path string) string {
	switch {
	case strings.HasSuffix(base, "/") && strings.HasPrefix(path, "/"):
		return base + path[1:]
	case !strings.HasSuffix(base, "/") && !strings.HasPrefix(path, "/"):
		return base + "/" + path
	default:
		return base + path
	}
}

// TokenValidator validates a Bearer token in-process and returns the
// session claims (Username, Groups, Departments). It is implemented by a
// thin adapter around internal/auth.Service.ValidateSession (see
// cmd/main.go) so the handler package stays free of Keycloak knowledge.
type TokenValidator func(ctx context.Context, token string) (*AuthClaims, bool)

// RequireAuthFunc is the in-process variant of RequireAuth: instead of
// calling an external session endpoint over HTTP it delegates token
// validation to the given validator. Behaviour (claims in context, public
// path bypass) is identical to RequireAuth.
func RequireAuthFunc(v TokenValidator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Health check, the auth service's own routes and the Mailpit
			// web inbox (single-port surface) never require a token.
			if r.URL.Path == "/health" || strings.HasPrefix(r.URL.Path, "/api/auth/") ||
				strings.HasPrefix(r.URL.Path, "/mailpit/") {
				next.ServeHTTP(w, r)
				return
			}
			authHeader := r.Header.Get("Authorization")
			if !strings.HasPrefix(authHeader, "Bearer ") {
				writeError(w, http.StatusUnauthorized, "missing or invalid Authorization header")
				return
			}
			token := strings.TrimPrefix(authHeader, "Bearer ")
			session, ok := v(r.Context(), token)
			if !ok || !session.Active {
				writeError(w, http.StatusUnauthorized, "token session is not active")
				return
			}
			ctx := context.WithValue(r.Context(), claimsContextKey, session)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireAuth returns an HTTP middleware that:
//  1. Extracts the Bearer token from the Authorization header.
//  2. Calls GET authBaseURL+sessionEndpoint with the same Authorization header.
//  3. Expects a 200 response whose JSON body has active=true.
//  4. Decodes the full session response (username, groups, department) and
//     stores it in the request context for downstream handlers.
//
// The /health path always bypasses authentication.
func RequireAuth(authBaseURL, sessionEndpoint string) func(http.Handler) http.Handler {
	targetURL := joinURL(authBaseURL, sessionEndpoint)
	client := &http.Client{Timeout: 5 * time.Second}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Health check, the auth service's own routes and the Mailpit
			// web inbox (single-port surface) never require a token.
			if r.URL.Path == "/health" || strings.HasPrefix(r.URL.Path, "/api/auth/") ||
				strings.HasPrefix(r.URL.Path, "/mailpit/") {
				next.ServeHTTP(w, r)
				return
			}

			authHeader := r.Header.Get("Authorization")
			if !strings.HasPrefix(authHeader, "Bearer ") {
				writeError(w, http.StatusUnauthorized, "missing or invalid Authorization header")
				return
			}
			// Always forward with the "Bearer " prefix, matching Java behaviour.
			token := strings.TrimPrefix(authHeader, "Bearer ")

			// Call the auth server session validation endpoint.
			// Set Accept: application/json — matches Java's MediaType.APPLICATION_JSON.
			req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, targetURL, nil)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "failed to build auth request")
				return
			}
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Accept", "application/json")

			resp, err := client.Do(req)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "auth server unreachable")
				return
			}
			defer resp.Body.Close()

			// Read the body once so we can both log it and decode it.
			bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
			if err != nil {
				writeError(w, http.StatusInternalServerError, "failed to read auth server response")
				return
			}

			slog.Default().Debug("auth server response",
				"url", targetURL,
				"status", resp.StatusCode,
				"body", string(bodyBytes),
			)

			if resp.StatusCode != http.StatusOK {
				slog.Default().Warn("auth server rejected token",
					"status", resp.StatusCode,
					"body", string(bodyBytes),
				)
				writeError(w, http.StatusUnauthorized, "invalid or expired token")
				return
			}

			var session AuthSessionResponse
			if err := json.Unmarshal(bodyBytes, &session); err != nil {
				slog.Default().Warn("malformed auth server response",
					"error", err,
					"body", string(bodyBytes),
				)
				writeError(w, http.StatusUnauthorized, "malformed token claims from auth server")
				return
			}

			// Mirror Java: only accept sessions where active == true.
			if !session.Active {
				writeError(w, http.StatusUnauthorized, "token session is not active")
				return
			}

			// Enrich with claims decoded directly from the JWT payload so that
			// preferred_username, groups, and department always reflect the
			// token, regardless of what the auth server returns in its body.
			// The auth server response has department=null; the real value
			// lives in the JWT as a flat string array: "department": ["1002"].
			if jc := decodeJWTPayload(token); jc.PreferredUsername != "" {
				session.Username = jc.PreferredUsername
				if len(jc.Groups) > 0 {
					session.Groups = jc.Groups
				}
				if len(jc.Department) > 0 {
					session.Departments = jc.Department
				}
			}

			ctx := context.WithValue(r.Context(), claimsContextKey, &session)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// jwtClaims holds the subset of JWT payload fields we care about.
type jwtClaims struct {
	PreferredUsername string   `json:"preferred_username"`
	Groups            []string `json:"groups"`
	// Department is a flat string slice in the token: "department": ["1002"]
	Department []string `json:"department"`
}

// decodeJWTPayload base64-decodes the payload segment of a JWT (without
// signature verification — validation is delegated to the auth server).
// Returns zero-value jwtClaims on any parse error.
func decodeJWTPayload(token string) jwtClaims {
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		return jwtClaims{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return jwtClaims{}
	}
	var c jwtClaims
	_ = json.Unmarshal(payload, &c)
	// Keycloak prefixes group names with "/". Strip it so callers can compare
	// plain names like "CustRelations" instead of "/CustRelations".
	for i, g := range c.Groups {
		c.Groups[i] = strings.TrimPrefix(g, "/")
	}
	return c
}

// claimsFromContext retrieves the auth claims stored by RequireAuth.
func claimsFromContext(ctx context.Context) (*AuthClaims, bool) {
	claims, ok := ctx.Value(claimsContextKey).(*AuthClaims)
	return claims, ok
}

// ClaimsFromContext is the exported form of claimsFromContext, for the
// in-process sub-services (internal/documents, internal/forms) that need
// the authenticated caller's claims.
func ClaimsFromContext(ctx context.Context) (*AuthClaims, bool) {
	return claimsFromContext(ctx)
}

// allowedDeptSet builds a set of allowed department codes from claims.
// Returns nil when the token carries no department restriction.
func allowedDeptSet(claims *AuthClaims) map[string]struct{} {
	if len(claims.Departments) == 0 {
		return nil
	}
	m := make(map[string]struct{}, len(claims.Departments))
	for _, c := range claims.Departments {
		m[c] = struct{}{}
	}
	return m
}

// filterByDepartment keeps only the instances whose GlobalContext department
// codes overlap with the caller's allowed department codes.
// When the token carries no department codes, all instances are returned unchanged.
func filterByDepartment(instances []*domain.ExecutionState, claims *AuthClaims) []*domain.ExecutionState {
	allowed := allowedDeptSet(claims)
	if allowed == nil {
		return instances
	}
	out := make([]*domain.ExecutionState, 0, len(instances))
	for _, inst := range instances {
		if instanceInDepartment(inst, allowed) {
			out = append(out, inst)
		}
	}
	return out
}

// instanceInDepartment checks whether the instance GlobalContext contains at
// least one department code present in the allowed set.
//
// Supports the nested object shape used by StartProcess request bodies:
//
//	"department": {"codes": ["1001", "1002"]}
//
// Also accepts flat arrays as a fallback:
//
//	"department": ["1001", "1002"]
//
// instanceInDepartment checks whether the instance carries at least one
// department code present in the allowed set. Department extraction is
// delegated to engine.InstanceDepartments (single source of truth for the
// department.codes / department / departments shapes).
func instanceInDepartment(inst *domain.ExecutionState, allowed map[string]struct{}) bool {
	for _, c := range engine.InstanceDepartments(inst) {
		if _, ok := allowed[c]; ok {
			return true
		}
	}
	return false
}
