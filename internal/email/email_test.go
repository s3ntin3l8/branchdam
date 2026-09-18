package email

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogSender_Send(t *testing.T) {
	sender := &logSender{log: slog.New(slog.DiscardHandler)}
	err := sender.Send(context.Background(), "user@example.com", "Test Subject", "<p>html</p>", "plain text")
	require.NoError(t, err)
}

// TestLogSender_StripsCRLFFromLoggedFields guards against CodeQL
// go/log-injection (alert #91): unlike smtpSender.Send, logSender never
// runs `to` through mail.ParseAddress, so a stored email/subject/body
// containing CR/LF must be stripped before it reaches slog, or an
// attacker-controlled value could forge extra log lines.
func TestLogSender_StripsCRLFFromLoggedFields(t *testing.T) {
	var buf strings.Builder
	sender := &logSender{log: slog.New(slog.NewJSONHandler(&buf, nil))}

	err := sender.Send(context.Background(),
		"victim@example.com\r\nlevel=ERROR msg=forged",
		"Subject\nInjected-Header: evil",
		"<p>html</p>",
		"body\r\ntext with\nnewlines",
	)
	require.NoError(t, err)

	var record map[string]any
	require.NoError(t, json.Unmarshal([]byte(buf.String()), &record))
	assert.Equal(t, "victim@example.comlevel=ERROR msg=forged", record["to"])
	assert.Equal(t, "SubjectInjected-Header: evil", record["subject"])
	assert.Equal(t, "bodytext withnewlines", record["body_preview"])
}

func TestBuildMessage(t *testing.T) {
	msg := buildMessage("sender@example.com", "recipient@example.com", "Test Subject", "<p>Hello</p>", "Hello")
	s := string(msg)

	assert.Contains(t, s, "From: sender@example.com")
	assert.Contains(t, s, "To: recipient@example.com")
	assert.Contains(t, s, "Subject: Test Subject")
	assert.Contains(t, s, "Content-Type: multipart/alternative")
	assert.Contains(t, s, "<p>Hello</p>")
	assert.Contains(t, s, "Hello")
}

func TestPasswordResetHTML(t *testing.T) {
	data := ResetEmailData{
		Username:  "alice",
		ResetLink: "https://example.com/reset?token=abc123",
		ExpiresAt: "24 hours",
	}
	html := PasswordResetHTML(data)
	assert.Contains(t, html, "alice")
	assert.Contains(t, html, "https://example.com/reset?token=abc123")
	assert.Contains(t, html, "24 hours")
	assert.Contains(t, html, "Reset Password")
	// No remote resources
	assert.NotContains(t, html, "http://")
	assert.NotContains(t, html, "<script")
	assert.NotContains(t, html, "<link")
}

func TestPasswordResetText(t *testing.T) {
	data := ResetEmailData{
		Username:  "bob",
		ResetLink: "https://example.com/reset?token=xyz",
		ExpiresAt: "12 hours",
	}
	text := PasswordResetText(data)
	assert.Contains(t, text, "bob")
	assert.Contains(t, text, "https://example.com/reset?token=xyz")
	assert.Contains(t, text, "12 hours")
	assert.Contains(t, text, "Password Reset")
}

func TestPasswordResetHTML_EscapesUserInput(t *testing.T) {
	data := ResetEmailData{
		Username:  `<script>alert("xss")</script>`,
		ResetLink: "https://example.com/reset?token=safe",
		ExpiresAt: "24 hours",
	}
	html := PasswordResetHTML(data)
	assert.NotContains(t, html, "<script>")
	assert.Contains(t, html, "&lt;script&gt;")
}

// TestPasswordResetHTML_StripsCRLFFromUsername and its Text counterpart
// guard against CodeQL go/email-content-injection (alert #86): a stored
// username containing CR/LF could otherwise inject extra lines into the
// rendered email body.
func TestPasswordResetHTML_StripsCRLFFromUsername(t *testing.T) {
	data := ResetEmailData{
		Username:  "alice\r\nBcc: attacker@evil.com",
		ResetLink: "https://example.com/reset?token=safe",
		ExpiresAt: "24 hours",
	}
	html := PasswordResetHTML(data)
	assert.NotContains(t, html, "\r")
	// CR/LF stripped means "Bcc: ..." merges onto the same line as the
	// greeting instead of starting a line of its own.
	assert.NotContains(t, html, "\nBcc: attacker@evil.com")
	assert.Contains(t, html, "aliceBcc: attacker@evil.com")
}

func TestPasswordResetText_StripsCRLFFromUsername(t *testing.T) {
	data := ResetEmailData{
		Username:  "bob\r\nBcc: attacker@evil.com",
		ResetLink: "https://example.com/reset?token=safe",
		ExpiresAt: "24 hours",
	}
	text := PasswordResetText(data)
	assert.NotContains(t, text, "\r")
	assert.NotContains(t, text, "\nBcc: attacker@evil.com")
	assert.Contains(t, text, "Hi bobBcc: attacker@evil.com,")
}

func TestSMTPSender_LocalSMTPFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping SMTP integration test in short mode")
	}

	// Start a local TCP listener that speaks minimal SMTP.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()

	addr := listener.Addr().(*net.TCPAddr)
	var mu sync.Mutex
	var received []string
	var mailFrom string

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go handleMockSMTP(conn, &mu, &received, &mailFrom)
		}
	}()

	sender := &smtpSender{
		cfg: Config{
			Provider: "smtp",
			Host:     "127.0.0.1",
			Port:     addr.Port,
			From:     "test@branchdam.local",
			TLS:      "none",
		},
		log: slog.New(slog.DiscardHandler),
	}

	err = sender.Send(context.Background(), "user@example.com", "Test", "<p>Hi</p>", "Hi")
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, received, 1)
	assert.Contains(t, received[0], "From: test@branchdam.local")
	assert.Contains(t, received[0], "To: user@example.com")
	assert.Contains(t, received[0], "Subject: Test")
	// MAIL FROM envelope is the bare angle-addr (RFC 5321), not the
	// display-name form. The From config is already bare here, but the
	// envelope path also goes through mail.ParseAddress + .Address.
	assert.Equal(t, "<test@branchdam.local>", mailFrom)
}

func TestSMTPSender_MailFromEnvelopeUsesAngleAddr(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping SMTP integration test in short mode")
	}

	// Regression for the display-name leakage: configuring
	// "branchDAM <noreply@example.com>" must NOT produce
	// MAIL FROM:<branchDAM <noreply@example.com>> (invalid RFC 5321).
	// The MIME From: header still renders the display-name form;
	// only the SMTP envelope is stripped to the angle-addr.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()

	addr := listener.Addr().(*net.TCPAddr)
	var mu sync.Mutex
	var received []string
	var mailFrom string

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go handleMockSMTP(conn, &mu, &received, &mailFrom)
		}
	}()

	sender := &smtpSender{
		cfg: Config{
			Provider: "smtp",
			Host:     "127.0.0.1",
			Port:     addr.Port,
			From:     "branchDAM <noreply@example.com>",
			TLS:      "none",
		},
		log: slog.New(slog.DiscardHandler),
	}

	err = sender.Send(context.Background(), "user@example.com", "Test", "<p>Hi</p>", "Hi")
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	// Envelope: just the angle-addr.
	assert.Equal(t, "<noreply@example.com>", mailFrom)
	// MIME header: still the display-name form, so the recipient's
	// mail client shows a real sender name.
	require.Len(t, received, 1)
	assert.Contains(t, received[0], "From: branchDAM <noreply@example.com>")
}

func handleMockSMTP(conn net.Conn, mu *mu, received *[]string, mailFrom *string) {
	defer func() { _ = conn.Close() }()

	// SmtpServer is the minimal mock.
	// We use a simple state machine: 220 -> accept EHLO/MAIL/RCPT/DATA -> 250
	buf := make([]byte, 4096)

	// Banner
	_, _ = conn.Write([]byte("220 mock SMTP\r\n"))

	var msg strings.Builder
	inData := false

	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		lines := strings.Split(string(buf[:n]), "\r\n")

		for _, line := range lines {
			if line == "" {
				continue
			}
			if inData {
				if line == "." {
					inData = false
					mu.Lock()
					*received = append(*received, msg.String())
					mu.Unlock()
					msg.Reset()
					_, _ = conn.Write([]byte("250 OK\r\n"))
				} else {
					msg.WriteString(line + "\r\n")
				}
				continue
			}

			upper := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(upper, "EHLO") || strings.HasPrefix(upper, "HELO"):
				_, _ = conn.Write([]byte("250-mock\r\n250 OK\r\n"))
			case strings.HasPrefix(upper, "MAIL FROM"):
				mu.Lock()
				// Capture the angle-addr argument only, not the
				// "MAIL FROM:" command token. The SMTP wire format
				// is "MAIL FROM:<addr>"; strip the verb. Preserve
				// the original case (we ToUpper'd for the switch).
				arg := line[len("MAIL FROM:"):]
				*mailFrom = arg
				mu.Unlock()
				_, _ = conn.Write([]byte("250 OK\r\n"))
			case strings.HasPrefix(upper, "RCPT TO"):
				_, _ = conn.Write([]byte("250 OK\r\n"))
			case strings.HasPrefix(upper, "DATA"):
				_, _ = conn.Write([]byte("354 Start mail input\r\n"))
				inData = true
			case strings.HasPrefix(upper, "QUIT"):
				_, _ = conn.Write([]byte("221 Bye\r\n"))
				return
			default:
				_, _ = conn.Write([]byte("250 OK\r\n"))
			}
		}
	}
}

type mu = sync.Mutex

func TestSMTPSender_RejectsCRLFInTo(t *testing.T) {
	sender := &smtpSender{
		cfg: Config{
			Provider: "smtp",
			Host:     "127.0.0.1",
			Port:     25,
			From:     "test@branchdam.local",
			TLS:      "none",
		},
		log: slog.New(slog.DiscardHandler),
	}
	err := sender.Send(context.Background(), "user@example.com\r\nBcc: attacker@example.com", "Test", "<p>Hi</p>", "Hi")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid to address")
}

func TestSMTPSender_RejectsCRLFInFrom(t *testing.T) {
	sender := &smtpSender{
		cfg: Config{
			Provider: "smtp",
			Host:     "127.0.0.1",
			Port:     25,
			From:     "test@branchdam.local\r\nBcc: attacker@example.com",
			TLS:      "none",
		},
		log: slog.New(slog.DiscardHandler),
	}
	err := sender.Send(context.Background(), "user@example.com", "Test", "<p>Hi</p>", "Hi")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid from address")
}

func TestSMTPSender_StartTLSFailsClosedWhenUnsupported(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping SMTP integration test in short mode")
	}

	// Start a local TCP listener that does NOT advertise STARTTLS.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()

	addr := listener.Addr().(*net.TCPAddr)

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_, _ = c.Write([]byte("220 mock SMTP\r\n"))
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					for _, line := range strings.Split(string(buf[:n]), "\r\n") {
						upper := strings.ToUpper(line)
						switch {
						case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
							// Deliberately omit STARTTLS from the EHLO response
							_, _ = c.Write([]byte("250-mock\r\n250 OK\r\n"))
						case strings.HasPrefix(upper, "QUIT"):
							_, _ = c.Write([]byte("221 Bye\r\n"))
							return
						default:
							_, _ = c.Write([]byte("250 OK\r\n"))
						}
					}
				}
			}(conn)
		}
	}()

	sender := &smtpSender{
		cfg: Config{
			Provider: "smtp",
			Host:     "127.0.0.1",
			Port:     addr.Port,
			From:     "test@branchdam.local",
			TLS:      "starttls",
			Username: "u",
			Password: "p",
		},
		log: slog.New(slog.DiscardHandler),
	}

	// tls=starttls but server didn't advertise STARTTLS -> must error
	// rather than silently fall through to plaintext AUTH.
	err = sender.Send(context.Background(), "user@example.com", "Test", "<p>Hi</p>", "Hi")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "STARTTLS")
}

func TestBuildMessage_Uses8BitTransferEncoding(t *testing.T) {
	msg := buildMessage("sender@example.com", "recipient@example.com", "Test Subject", "<p>Hi</p>", "Hi")
	s := string(msg)
	assert.Contains(t, s, "Content-Transfer-Encoding: 8bit")
	// Both text/plain and text/html parts
	assert.Equal(t, 2, strings.Count(s, "Content-Transfer-Encoding: 8bit"))
	// And NO quoted-printable anywhere (issue 3: CTE used to lie about QP)
	assert.NotContains(t, s, "quoted-printable")
}
