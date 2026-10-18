package auth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Outbound mail (activation links, quota top-up claims).
//
// Sender priority:
//  1. SMTP — the realm's SMTP settings as configured in the Keycloak local
//     administration (Realm settings → Email → SMTP Server Host/Port/From,
//     read live via the Admin API so the admin console is the single source
//     of truth). Locally this is the Mailpit sink (host: mailpit, port: 1025).
//  2. HTTP email gateway (ACTIVATION_EMAIL_GATEWAY / MCP_GMAIL_GATEWAY).
//  3. Log mode (nothing configured) — the mail is logged, status "logged".
// ---------------------------------------------------------------------------

// smtpConfig is a subset of Keycloak realm smtpSettings.
type smtpConfig struct {
	Host     string
	Port     int
	From     string
	StartTLS bool
	SSL      bool
	User     string
	Password string
}

// smtpFromKeycloak fetches the realm's SMTP settings from the Keycloak
// Admin API. Returns (nil, nil) when SMTP is not configured (empty host).
func (s *Service) smtpFromKeycloak(ctx context.Context) (*smtpConfig, error) {
	token, err := s.adminToken(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := s.adminGet(ctx, "/admin/realms/"+s.realm, token)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("keycloak returned %d for realm settings", resp.StatusCode)
	}
	var realm struct {
		SMTPSettings struct {
			Host     string `json:"host"`
			Port     string `json:"port"`
			From     string `json:"from"`
			StartTLS bool   `json:"starttls"`
			SSL      bool   `json:"ssl"`
			User     string `json:"user"`
			Password string `json:"password"`
		} `json:"smtpSettings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&realm); err != nil {
		return nil, err
	}
	cfg := realm.SMTPSettings
	if strings.TrimSpace(cfg.Host) == "" {
		return nil, nil
	}
	port := 25
	if p, err := strconv.Atoi(strings.TrimSpace(cfg.Port)); err == nil && p > 0 {
		port = p
	}
	if cfg.From == "" {
		cfg.From = "oumma@localhost"
	}
	return &smtpConfig{
		Host: strings.TrimSpace(cfg.Host), Port: port, From: cfg.From,
		StartTLS: cfg.StartTLS, SSL: cfg.SSL,
		User: strings.TrimSpace(cfg.User), Password: cfg.Password,
	}, nil
}

// SendMail delivers a plain-text mail. Returns "email_sent" (SMTP or
// gateway) or "logged" (development log mode).
// SMTP source priority: Keycloak realm settings (admin console) → SMTP_*
// environment variables (docker compose) → HTTP gateway → log mode.
func (s *Service) SendMail(ctx context.Context, to, subject, body string) (string, error) {
	cfg, err := s.smtpFromKeycloak(ctx)
	if err != nil {
		s.log.Warn("keycloak smtp settings unavailable", "error", err)
	}
	if cfg == nil {
		cfg = smtpFromEnv()
	}
	if cfg != nil {
		if err := sendMailSMTP(ctx, cfg, to, subject, body); err != nil {
			s.log.Error("SMTP send failed", "host", cfg.Host, "port", cfg.Port, "to", to, "error", err)
			return "", fmt.Errorf("smtp send failed: %w", err)
		}
		s.log.Info("mail sent via SMTP", "host", cfg.Host, "port", cfg.Port, "to", to, "subject", subject)
		return "email_sent", nil
	}
	return sendMailViaGateway(ctx, s.log, s.http, to, subject, body)
}

// smtpFromEnv builds an SMTP config from SMTP_* environment variables
// (set in docker compose; locally: the Mailpit sink).
func smtpFromEnv() *smtpConfig {
	host := strings.TrimSpace(os.Getenv("SMTP_HOST"))
	if host == "" {
		return nil
	}
	port := 25
	if p, err := strconv.Atoi(strings.TrimSpace(os.Getenv("SMTP_PORT"))); err == nil && p > 0 {
		port = p
	}
	from := strings.TrimSpace(os.Getenv("SMTP_FROM"))
	if from == "" {
		from = "oumma@localhost"
	}
	return &smtpConfig{
		Host:     host,
		Port:     port,
		From:     from,
		StartTLS: strings.EqualFold(os.Getenv("SMTP_STARTTLS"), "true"),
		SSL:      strings.EqualFold(os.Getenv("SMTP_SSL"), "true"),
		User:     strings.TrimSpace(os.Getenv("SMTP_USER")),
		Password: os.Getenv("SMTP_PASSWORD"),
	}
}

// sendMailViaGateway posts {from,to,subject,body} to the configured HTTP
// email gateway; without a gateway it logs the mail (development mode).
func sendMailViaGateway(ctx context.Context, log *slog.Logger, client *http.Client, to, subject, body string) (string, error) {
	gateway := strings.TrimRight(os.Getenv("ACTIVATION_EMAIL_GATEWAY"), "/")
	if gateway == "" {
		gateway = strings.TrimRight(os.Getenv("MCP_GMAIL_GATEWAY"), "/")
	}
	if gateway == "" {
		log.Info("mail (no SMTP/gateway configured — log mode)", "to", to, "subject", subject, "body", body)
		return "logged", nil
	}
	payload, _ := json.Marshal(map[string]any{
		"from":    os.Getenv("ACTIVATION_EMAIL_FROM"),
		"to":      to,
		"subject": subject,
		"body":    body,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway, strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("email gateway returned %d", resp.StatusCode)
	}
	return "email_sent", nil
}

// sendMailSMTP delivers via net/smtp. Mailpit (1025) needs no auth/TLS;
// real servers use implicit SSL (465) or STARTTLS (587) + basic auth.
func sendMailSMTP(ctx context.Context, cfg *smtpConfig, to, subject, body string) error {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	msg := buildMailMessage(cfg.From, to, subject, body)
	dialer := &net.Dialer{Timeout: 15 * time.Second}

	if cfg.SSL {
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: cfg.Host})
		if err != nil {
			return err
		}
		c, err := smtp.NewClient(conn, cfg.Host)
		if err != nil {
			return err
		}
		defer c.Close()
		if cfg.User != "" {
			if err := c.Auth(smtp.PlainAuth("", cfg.User, cfg.Password, cfg.Host)); err != nil {
				return err
			}
		}
		if err := c.Mail(cfg.From); err != nil {
			return err
		}
		if err := c.Rcpt(to); err != nil {
			return err
		}
		w, err := c.Data()
		if err != nil {
			return err
		}
		if _, err := w.Write([]byte(msg)); err != nil {
			return err
		}
		return w.Close()
	}

	var auth smtp.Auth
	if cfg.User != "" {
		auth = smtp.PlainAuth("", cfg.User, cfg.Password, cfg.Host)
	}
	return smtp.SendMail(addr, auth, cfg.From, []string{to}, []byte(msg))
}

// buildMailMessage renders a minimal RFC 5322 text/plain message.
func buildMailMessage(from, to, subject, body string) string {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + subject + "\r\n")
	b.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(body)
	return b.String()
}
