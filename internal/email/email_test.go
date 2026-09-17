package email

import (
	"context"
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

func TestSMTPSender_LocalSMTPFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping SMTP integration test in short mode")
	}

	// Start a local TCP listener that speaks minimal SMTP.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	addr := listener.Addr().(*net.TCPAddr)
	var mu sync.Mutex
	var received []string

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go handleMockSMTP(conn, &mu, &received)
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
}

func handleMockSMTP(conn net.Conn, mu *mu, received *[]string) {
	defer conn.Close()

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
