package email

import (
	"fmt"
	"html"
	"strings"
)

// ResetEmailData holds the template variables for the password-reset
// email. All fields are user-controlled except ResetLink.
type ResetEmailData struct {
	Username  string
	ResetLink string
	ExpiresAt string
}

// PasswordResetHTML returns the HTML body for the password-reset email.
// The template is strict: no remote images, no scripts, no stylesheets
// beyond inline styles. User-controlled fields (Username) have CR/LF/NUL
// stripped (sanitizeControlChars) before HTML-escaping, so a stored
// username can't inject extra lines into the rendered message body
// (CodeQL go/email-content-injection).
func PasswordResetHTML(data ResetEmailData) string {
	escapedUser := html.EscapeString(sanitizeControlChars(data.Username))
	escapedLink := html.EscapeString(data.ResetLink)
	escapedExpiry := html.EscapeString(data.ExpiresAt)

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Reset your password</title>
</head>
<body style="margin:0;padding:0;background-color:#1a1a1a;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;">
<table width="100%%" cellpadding="0" cellspacing="0" style="background-color:#1a1a1a;padding:40px 20px;">
<tr><td align="center">
<table width="100%%" cellpadding="0" cellspacing="0" style="max-width:480px;background-color:#262626;border-radius:8px;border:1px solid #404040;">
<tr><td style="padding:32px;">
<h1 style="margin:0 0 8px;font-size:20px;color:#f5f5f5;">Password Reset</h1>
<p style="margin:0 0 24px;font-size:14px;color:#a3a3a3;">Hi %s,</p>
<p style="margin:0 0 24px;font-size:14px;color:#d4d4d4;line-height:1.5;">
Someone requested a password reset for your branchDAM account.
If this was you, click the button below to set a new password.
</p>
<table width="100%%" cellpadding="0" cellspacing="0"><tr><td align="center" style="padding:0 0 24px;">
<a href="%s" style="display:inline-block;background-color:#3b82f6;color:#ffffff;font-size:14px;font-weight:600;text-decoration:none;padding:12px 24px;border-radius:6px;">
Reset Password
</a>
</td></tr></table>
<p style="margin:0 0 8px;font-size:13px;color:#a3a3a3;">
This link expires in %s. If you didn't request this, you can safely ignore this email.
</p>
<p style="margin:0;font-size:12px;color:#737373;">
If the button doesn't work, copy and paste this URL into your browser:<br>
<span style="word-break:break-all;color:#60a5fa;">%s</span>
</p>
</td></tr>
<tr><td style="padding:16px 32px;border-top:1px solid #404040;">
<p style="margin:0;font-size:12px;color:#737373;">
Sent by branchDAM
</p>
</td></tr>
</table>
</td></tr>
</table>
</body>
</html>`, escapedUser, escapedLink, escapedExpiry, escapedLink)
}

// PasswordResetText returns the plain-text fallback for the
// password-reset email. No HTML, no markup, just the essentials.
// User-controlled fields (Username) have CR/LF/NUL stripped
// (sanitizeControlChars): there's no markup to escape into here, but
// an unstripped username could still inject extra lines into the body.
func PasswordResetText(data ResetEmailData) string {
	username := sanitizeControlChars(data.Username)
	var b strings.Builder
	fmt.Fprintf(&b, "Password Reset\n\n")
	fmt.Fprintf(&b, "Hi %s,\n\n", username)
	fmt.Fprintf(&b, "Someone requested a password reset for your branchDAM account.\n")
	fmt.Fprintf(&b, "If this was you, visit the link below to set a new password:\n\n")
	fmt.Fprintf(&b, "%s\n\n", data.ResetLink)
	fmt.Fprintf(&b, "This link expires in %s.\n", data.ExpiresAt)
	fmt.Fprintf(&b, "If you didn't request this, you can safely ignore this email.\n")
	return b.String()
}
