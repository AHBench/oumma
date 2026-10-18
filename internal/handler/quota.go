package handler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Token quota for all users (role-based monthly budget).
//
// Normal (non-Designer) users get NORMAL_USER_TOKEN_QUOTA (default 20 000)
// monthly LLM tokens; Designer users get DESIGNER_TOKEN_QUOTA (default
// 50 000). Both are configurable in the docker compose file. When the
// budget is exhausted the app tells the user the quota is finished and
// offers a "claim" action, which emails the administration
// (QUOTA_CLAIM_EMAIL env, default contact@brief-dz.com) a top-up request.
// Setting a quota env to 0 makes that role unlimited.
// ---------------------------------------------------------------------------

const defaultNormalUserTokenQuota = 20000
const defaultDesignerTokenQuota = 50000
const defaultQuotaClaimEmail = "contact@brief-dz.com"

func quotaFromEnv(name string, def int64) int64 {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	var n int64
	if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
		return n // 0 = unlimited
	}
	return def
}

func normalUserTokenQuota() int64 { return quotaFromEnv("NORMAL_USER_TOKEN_QUOTA", defaultNormalUserTokenQuota) }
func designerTokenQuota() int64   { return quotaFromEnv("DESIGNER_TOKEN_QUOTA", defaultDesignerTokenQuota) }

// quotaLimitFor returns the caller's monthly budget (0 = unlimited).
func quotaLimitFor(claims *AuthClaims) int64 {
	if hasDesignerGroup(claims.Groups) {
		return designerTokenQuota()
	}
	return normalUserTokenQuota()
}

func hasDesignerGroup(groups []string) bool {
	return sliceContains(groups, "Designer")
}

// quotaState computes the caller's quota state (role-based budget).
func (h *Handler) quotaState(ctx context.Context, claims *AuthClaims) (map[string]any, error) {
	limit := quotaLimitFor(claims)
	unlimited := limit <= 0
	out := map[string]any{
		"unlimited": unlimited,
		"limit":     0,
		"used":      0,
		"remaining": 0,
		"exhausted": false,
		"month":     time.Now().UTC().Format("2006-01"),
	}
	if claims.Username == "" {
		return out, nil
	}
	p, c, _, err := h.engine.TokenUsageByUsernameMonth(ctx, claims.Username, time.Date(time.Now().UTC().Year(), time.Now().UTC().Month(), 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		return nil, err
	}
	used := p + c
	out["used"] = used
	if unlimited {
		return out, nil
	}
	out["limit"] = limit
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	out["remaining"] = remaining
	out["exhausted"] = used >= limit
	return out, nil
}

// EnforceQuota blocks LLM-consuming calls when the caller's monthly budget
// is exhausted (role-based: normal 20k, designer 50k by default).
// Returns true (and writes the 429 response) when the call must be refused.
func (h *Handler) EnforceQuota(w http.ResponseWriter, r *http.Request, claims *AuthClaims) bool {
	if claims == nil {
		return false
	}
	state, err := h.quotaState(r.Context(), claims)
	if err != nil {
		slog.Warn("quota check failed", "error", err)
		return false // fail open: a quota bug must not break the agent
	}
	if exhausted, _ := state["exhausted"].(bool); exhausted {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error":   "quota_exhausted",
			"message": fmt.Sprintf("Monthly token quota exhausted (%v used of %v). Claim a top-up from the administration.", state["used"], state["limit"]),
			"limit":   state["limit"],
			"used":    state["used"],
		})
		return true
	}
	return false
}

// QuotaStatus handles GET /api/quota — the caller's quota state.
func (h *Handler) QuotaStatus(w http.ResponseWriter, r *http.Request) {
	claims, ok := claimsFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing claims")
		return
	}
	state, err := h.quotaState(r.Context(), claims)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// ClaimQuota handles POST /api/quota/claim — emails the administration a
// top-up request for the caller. Reuses the activation email gateway
// (ACTIVATION_EMAIL_GATEWAY / MCP_GMAIL_GATEWAY); without a gateway the
// request is logged (development mode) and still reported as sent.
func (h *Handler) ClaimQuota(w http.ResponseWriter, r *http.Request) {
	claims, ok := claimsFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "missing claims")
		return
	}
	state, err := h.quotaState(r.Context(), claims)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	to := strings.TrimSpace(os.Getenv("QUOTA_CLAIM_EMAIL"))
	if to == "" {
		to = defaultQuotaClaimEmail
	}
	dept := ""
	if len(claims.Departments) > 0 {
		dept = claims.Departments[0]
	}
	limitText, _ := state["limit"].(int64)
	if limitText == 0 {
		limitText = -1
	}
	body := fmt.Sprintf(
		"Quota top-up request\n\n"+
			"User: %s\n"+
			"Department: %s\n"+
			"Month: %v\n"+
			"Tokens used this month: %v\n"+
			"Monthly quota: %s\n\n"+
			"The user above has exhausted their monthly token quota in Oumma and "+
			"requests a top-up so they can keep using the AI features. Please review "+
			"and grant additional quota.\n",
		claims.Username, valueOr(dept, "n/a"), state["month"], state["used"],
		func() string {
			if limitText < 0 {
				return "unlimited"
			}
			return fmt.Sprintf("%d", limitText)
		}())
	if h.mailer == nil {
		writeError(w, http.StatusServiceUnavailable, "Mail service is not configured")
		return
	}
	status, err := h.mailer(r.Context(), to, "Oumma quota top-up request — "+claims.Username, body)
	if err != nil {
		slog.Warn("quota claim email failed", "user", claims.Username, "error", err)
		writeError(w, http.StatusBadGateway, "The claim request could not be sent right now. Please try again.")
		return
	}
	slog.Info("quota claim sent", "user", claims.Username, "to", to, "status", status)
	writeJSON(w, http.StatusOK, map[string]any{"sent": true, "status": status, "to": to})
}

func valueOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
