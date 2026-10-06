package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/s3ntin3l8/branchdam/internal/config"
)

func clientIPServer(trusted []string) *Server {
	return &Server{cfgProvider: staticConfigProvider{cfg: &config.Config{HTTP: config.HTTP{TrustedProxies: trusted}}}}
}

func clientIPReq(remote, xff string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/login", nil)
	r.RemoteAddr = remote
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

func TestClientIP_IgnoresXFFWithoutTrustedProxies(t *testing.T) {
	assert.Equal(t, "203.0.113.9", clientIP(clientIPServer(nil), clientIPReq("203.0.113.9:1234", "1.2.3.4")))
}

func TestClientIP_IgnoresXFFFromUntrustedPeer(t *testing.T) {
	s := clientIPServer([]string{"10.0.0.0/8"})
	assert.Equal(t, "203.0.113.9", clientIP(s, clientIPReq("203.0.113.9:1234", "1.2.3.4")))
}

// A client-supplied leftmost XFF entry must not be believed: Traefik
// appends the peer it saw, so the rightmost non-proxy hop is the client.
func TestClientIP_UsesRightmostUntrustedHop(t *testing.T) {
	s := clientIPServer([]string{"10.0.0.0/8"})
	r := clientIPReq("10.0.0.5:4321", "6.6.6.6, 198.51.100.7")
	assert.Equal(t, "198.51.100.7", clientIP(s, r), "spoofed leftmost entry must be ignored")
}

func TestClientIP_SkipsTrustedProxyHops(t *testing.T) {
	s := clientIPServer([]string{"10.0.0.0/8"})
	r := clientIPReq("10.0.0.5:4321", "198.51.100.7, 10.0.0.9")
	assert.Equal(t, "198.51.100.7", clientIP(s, r))
}

func TestClientIP_WildcardTrustUsesRightmostEntry(t *testing.T) {
	s := clientIPServer([]string{"*"})
	assert.Equal(t, "1.1.1.1", clientIP(s, clientIPReq("10.0.0.5:4321", "1.1.1.1")))
	assert.Equal(t, "198.51.100.7", clientIP(s, clientIPReq("10.0.0.5:4321", "6.6.6.6, 198.51.100.7")), "spoofed leftmost entry ignored")
}

func TestClientIP_GarbageXFFFallsBackToPeer(t *testing.T) {
	s := clientIPServer([]string{"10.0.0.0/8"})
	assert.Equal(t, "10.0.0.5", clientIP(s, clientIPReq("10.0.0.5:4321", "not-an-ip")))
}
