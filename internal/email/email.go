// Package email provides outbound email delivery for branchDAM.
//
// The Notifier interface is the seam for transactional-email providers
// (Resend, Postmark, SES, Mailgun). The smtpSender implementation
// uses net/smtp for self-hosted or relay-based delivery; the logSender
// implementation writes to slog for development and testing.
package email

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"strings"
)

// Notifier sends transactional email. Implementations must be safe
// for concurrent use. The context controls cancellation and timeout;
// implementations should honor it (e.g., SMTP dial timeout).
type Notifier interface {
	Send(ctx context.Context, to, subject, htmlBody, textBody string) error
}

// Config holds the email delivery configuration. Provider selects
// the implementation: "smtp" for real delivery, "log" for slog-only.
type Config struct {
	Provider string `yaml:"provider"` // "smtp" | "log" — default "log"
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`     // default 587
	Username string `yaml:"username"` // optional; empty = no AUTH
	Password string `yaml:"password"` // supports ${VAR} expansion
	From     string `yaml:"from"`     // sender address, e.g. "branchdam <noreply@example.com>"
	TLS      string `yaml:"tls"`      // "starttls" (default) | "implicit" | "none"
}

// New creates a Notifier from the given config. Returns a logSender
// when provider is "log" or empty, and an smtpSender when "smtp".
func New(cfg Config, log *slog.Logger) Notifier {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	switch strings.ToLower(cfg.Provider) {
	case "smtp":
		return newSMTPSender(cfg, log)
	default:
		return &logSender{log: log}
	}
}

// logSender writes email details to slog. Used when no SMTP is
// configured (provider=log or empty). Safe for concurrent use.
type logSender struct {
	log *slog.Logger
}

func (s *logSender) Send(_ context.Context, to, subject, htmlBody, textBody string) error {
	s.log.Warn("email: not sent (log-only mode)",
		"to", to,
		"subject", subject,
		"body_preview", truncate(textBody, 200),
	)
	return nil
}

// smtpSender delivers email via net/smtp. It supports STARTTLS,
// implicit TLS (port 465), and plain (no TLS) modes.
type smtpSender struct {
	cfg  Config
	log  *slog.Logger
	auth smtp.Auth
}

func newSMTPSender(cfg Config, log *slog.Logger) *smtpSender {
	var auth smtp.Auth
	if cfg.Username != "" {
		auth = smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	return &smtpSender{cfg: cfg, log: log, auth: auth}
}

func (s *smtpSender) Send(ctx context.Context, to, subject, htmlBody, textBody string) error {
	addr := net.JoinHostPort(s.cfg.Host, fmt.Sprintf("%d", s.cfg.Port))

	// Build the raw MIME message.
	msg := buildMessage(s.cfg.From, to, subject, htmlBody, textBody)

	// Dial with context-aware timeout.
	var conn net.Conn
	var err error
	dialer := net.Dialer{}

	if s.cfg.TLS == "implicit" {
		// Implicit TLS (port 465): wrap with TLS first.
		conn, err = tls.DialWithDialer(&dialer, "tcp", addr, &tls.Config{
			ServerName: s.cfg.Host,
		})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("email: dial %s: %w", addr, err)
	}
	defer conn.Close()

	client, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return fmt.Errorf("email: smtp client: %w", err)
	}
	defer client.Close()

	// STARTTLS if requested and server supports it.
	if s.cfg.TLS != "implicit" && s.cfg.TLS != "none" {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: s.cfg.Host}); err != nil {
				return fmt.Errorf("email: starttls: %w", err)
			}
		}
	}

	if s.auth != nil {
		if err := client.Auth(s.auth); err != nil {
			return fmt.Errorf("email: auth: %w", err)
		}
	}

	if err := client.Mail(s.cfg.From); err != nil {
		return fmt.Errorf("email: mail from: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("email: rcpt to: %w", err)
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("email: data: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("email: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("email: close data: %w", err)
	}

	s.log.Info("email: sent", "to", to, "subject", subject)
	return client.Quit()
}

// buildMessage constructs a MIME multipart/alternative message with
// text/plain and text/html parts. The boundary is hardcoded for
// determinism (no user-controlled content in the boundary).
func buildMessage(from, to, subject, htmlBody, textBody string) []byte {
	const boundary = "branchdam-reset-boundary"
	var b strings.Builder

	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + subject + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n")
	b.WriteString("\r\n")

	// text/plain part
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n")
	b.WriteString("\r\n")
	b.WriteString(textBody)
	b.WriteString("\r\n\r\n")

	// text/html part
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/html; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n")
	b.WriteString("\r\n")
	b.WriteString(htmlBody)
	b.WriteString("\r\n\r\n")

	b.WriteString("--" + boundary + "--\r\n")
	return []byte(b.String())
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
