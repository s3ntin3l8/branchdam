package auth

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequireAdmin(t *testing.T) {
	tests := []struct {
		name          string
		allowedGroups []string
		principal     *Principal
		method        string
		wantCode      int
		wantDetail    string
	}{
		{
			name:          "no principal in context",
			allowedGroups: []string{"admin"},
			principal:     nil,
			method:        http.MethodPost,
			wantCode:      http.StatusForbidden,
			wantDetail:    "authentication required",
		},
		{
			name:          "non-admin user POST denied",
			allowedGroups: []string{"admin"},
			principal: &Principal{
				Kind:   KindUser,
				Name:   "alice",
				Groups: []string{"users"},
			},
			method:     http.MethodPost,
			wantCode:   http.StatusForbidden,
			wantDetail: "admin authorization required",
		},
		{
			name:          "admin user POST allowed",
			allowedGroups: []string{"admin"},
			principal: &Principal{
				Kind:   KindUser,
				Name:   "bob",
				Groups: []string{"users", "admin"},
			},
			method:   http.MethodPost,
			wantCode: http.StatusOK,
		},
		{
			name:          "non-admin user GET allowed",
			allowedGroups: []string{"admin"},
			principal: &Principal{
				Kind:   KindUser,
				Name:   "alice",
				Groups: []string{"users"},
			},
			method:   http.MethodGet,
			wantCode: http.StatusOK,
		},
		{
			name:          "empty allowedGroups permits non-admin POST",
			allowedGroups: nil,
			principal: &Principal{
				Kind:   KindUser,
				Name:   "charlie",
				Groups: []string{"users"},
			},
			method:   http.MethodPost,
			wantCode: http.StatusOK,
		},
		{
			name:          "machine principal POST allowed",
			allowedGroups: []string{"admin"},
			principal: &Principal{
				Kind: KindMachine,
			},
			method:   http.MethodPost,
			wantCode: http.StatusOK,
		},
	}

	okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logBuf, nil))

			middleware := RequireAdmin(tt.allowedGroups, logger)
			handler := middleware(okHandler)

			req := httptest.NewRequest(tt.method, "/api/v1/scan", nil)
			if tt.principal != nil {
				req = req.WithContext(withPrincipal(req.Context(), *tt.principal))
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}

			if tt.wantCode == http.StatusForbidden {
				contentType := rec.Header().Get("Content-Type")
				if contentType != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", contentType)
				}

				var body errorResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("unmarshal error response: %v", err)
				}

				if body.Status != http.StatusForbidden {
					t.Errorf("body.Status = %d, want %d", body.Status, http.StatusForbidden)
				}
				if body.Title != "Forbidden" {
					t.Errorf("body.Title = %q, want Forbidden", body.Title)
				}
				if tt.wantDetail != "" && body.Detail != tt.wantDetail {
					t.Errorf("body.Detail = %q, want %q", body.Detail, tt.wantDetail)
				}
			}

			if len(tt.allowedGroups) == 0 {
				if !strings.Contains(logBuf.String(), "authz.groups is empty") {
					t.Errorf("expected warning log about empty authz.groups, got: %s", logBuf.String())
				}
			}
		})
	}
}
