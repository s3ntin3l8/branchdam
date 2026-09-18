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
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// Notifier sends transactional email. Implementations must be safe
// for concurrent use. The context controls cancellation and timeout;
// implementations must honor it (SMTP: dial timeout + per-conn
// deadline derived from ctx).
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
	// BaseURL is the public-facing base URL used to build links in
	// outbound email (e.g. password-reset links). When empty, the caller
	// is expected to fall back to deriving it from the request -- which
	// trusts the Host header. See config.AuthEmail.BaseURL.
	BaseURL string `yaml:"baseURL"`
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

// Send is the log-only delivery path. The to/subject/body fields are
// user- or admin-derived (a stored user email address for `to`, an
// operator-localized template for `subject`, a rendered HTML/text
// body) and intentionally logged at WARN so a development operator
// running without an SMTP server still sees the rendered message.
//
// codeql[go/log-injection]: the `to`, `subject`, and body_preview
// values are intentionally included in the log line so that a dev
// operator without an SMTP server can verify what would have been
// sent. `to` is a stored user email and is format-validated in the
// smtpSender.Send path (mail.ParseAddress); here we just log it.
func (s *logSender) Send(_ context.Context, to, subject, htmlBody, textBody string) error {
	s.log.Warn("email: not sent (log-only mode)",
		"to", to,
		"subject", subject,
		"body_preview", truncate(textBody, 200),
	)
	// htmlBody is unused by design: the dev-mode preview is the
	// plain-text part, which is shorter and easier to eyeball than
	// the HTML version. Kept in the signature to satisfy Notifier.
	_ = htmlBody
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

// sendTimeout is the upper bound on a single Send call (dial + MAIL +
// RCPT + DATA + Quit). The deadline is derived from ctx so callers can
// shorten it per-request; absent a ctx deadline, sendTimeout is used.
// smtp.Client does not honor ctx on TLS handshake, MAIL/RCPT/DATA,
// Write, or Quit, so we additionally wrap the conn in SetDeadline to
// avoid a stalled server hanging the password-reset request handler.
const sendTimeout = 30 * time.Second

// SendTimeout is exported so background goroutines that have detached
// from a request context (e.g. password_reset.go's deferred email
// delivery) can apply the same upper bound without re-deriving it.
const SendTimeout = sendTimeout

func (s *smtpSender) Send(ctx context.Context, to, subject, htmlBody, textBody string) error {
	// Validate From + To with mail.ParseAddress BEFORE doing any I/O.
	// mail.ParseAddress rejects bare CR/LF and other header-injection
	// shapes, so a stored user email containing "\r\nBcc: attacker@"
	// fails closed here instead of injecting SMTP envelopes. We also
	// need the parsed From below -- RFC 5321's MAIL FROM envelope takes
	// an angle-addr (just "user@domain"), NOT the display-name form
	// ("branchDAM <user@domain>") that the MIME From: header carries.
	parsedFrom, err := mail.ParseAddress(s.cfg.From)
	if err != nil {
		return fmt.Errorf("email: invalid from address %q: %w", s.cfg.From, err)
	}
	envelopeFrom := parsedFrom.Address
	if _, err := mail.ParseAddress(to); err != nil {
		return fmt.Errorf("email: invalid to address %q: %w", to, err)
	}

	addr := net.JoinHostPort(s.cfg.Host, fmt.Sprintf("%d", s.cfg.Port))

	// Compute a deadline derived from ctx (falling back to sendTimeout
	// when ctx has no deadline). We apply it via SetDeadline on the
	// underlying conn -- smtp.Client uses the conn's own deadlines,
	// not ctx, after the initial dial.
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(sendTimeout)
	}

	// Build the raw MIME message.
	msg := buildMessage(s.cfg.From, to, subject, htmlBody, textBody)

	// Dial with context-aware timeout.
	var conn net.Conn
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
	defer func() { _ = conn.Close() }()

	// Apply the deadline to the underlying conn so smtp.Client's
	// subsequent reads/writes (EHLO, STARTTLS, AUTH, MAIL, RCPT, DATA,
	// Quit) cannot outrun it. A stalled SMTP server now fails the
	// request instead of hanging the password-reset handler.
	if err := conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("email: set conn deadline: %w", err)
	}

	client, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return fmt.Errorf("email: smtp client: %w", err)
	}
	defer func() { _ = client.Close() }()

	// STARTTLS if requested and server supports it. When tls=starttls
	// (the default) and the server doesn't advertise STARTTLS, we
	// MUST fail closed: continuing on plaintext would expose the
	// AUTH credentials in the clear on the next step. The previous
	// behavior was a silent downgrade -- a real-world smtp.gmail.com
	// or smtp.sendgrid.net advertises STARTTLS, so this only fires
	// on misconfigured relays or stripped-down test servers, which
	// is precisely the case where we want to refuse to send.
	if s.cfg.TLS != "implicit" && s.cfg.TLS != "none" {
		supported, _ := client.Extension("STARTTLS")
		if !supported {
			return errors.New("email: server does not advertise STARTTLS but tls=starttls is configured; refusing to send credentials in plaintext")
		}
		if err := client.StartTLS(&tls.Config{ServerName: s.cfg.Host}); err != nil {
			return fmt.Errorf("email: starttls: %w", err)
		}
	}

	if s.auth != nil {
		if err := client.Auth(s.auth); err != nil {
			return fmt.Errorf("email: auth: %w", err)
		}
	}

	if err := client.Mail(envelopeFrom); err != nil {
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
//
// codeql[go/email-content-injection]: the message body intentionally
// contains user-derived values (the username greeting, the reset
// link with a per-request token, the formatted expiry). These are
// rendered through PasswordResetHTML / PasswordResetText, which
// html.EscapeString every user-controlled field (Username, ResetLink,
// ExpiresAt) before they reach this function. The reset link is built
// from a baseURL + a crypto/rand-generated token, neither of which is
// attacker-controllable through stored user fields. The boundary is a
// hardcoded constant, so no user input crosses into the MIME
// structure. mail.ParseAddress() in smtpSender.Send rejects any
// CR/LF in From/To before we get here, so header injection is also
// closed.
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
	// 8bit transfer encoding: the body is written raw UTF-8 (no QP
	// encoding). Declaring quoted-printable here without actually QP-
	// encoding the body would let any '=' in the reset URL or baseURL
	// corrupt the rendered email at QP-decoding receivers (a literal
	// '=' becomes "=3D"; a long line triggers a soft line break).
	// 8bit is honest: we send raw UTF-8 octets and rely on the
	// receiver's MIME parser to handle them.
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(textBody)
	b.WriteString("\r\n\r\n")

	// text/html part
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/html; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
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
