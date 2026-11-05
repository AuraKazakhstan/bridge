package routes

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yesmukhamedov/bridge/internal/config"
)

func TestContractSyncAccessFixedPolicyAndPropagation(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{200, "true"}, {200, "false"}, {401, "unauthorized"}, {403, "forbidden"}, {500, "failure"}, {200, "malformed"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/core/abac/check-access" {
					t.Errorf("unexpected upstream route: %s %s", r.Method, r.URL.Path)
				}
				if r.URL.RawQuery != "transaction=MMEMC&restriction=contract&action=sync" {
					t.Errorf("policy query was not fixed: %s", r.URL.RawQuery)
				}
				if r.Header.Get("Authorization") != "Bearer caller-token" {
					t.Errorf("caller authorization not preserved")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer upstream.Close()
			mux := http.NewServeMux()
			mountAituRoutes(mux, &config.Config{CoreURL: upstream.URL, UpstreamTimeout: time.Second})
			req := httptest.NewRequest("GET", "/contracts/sync-access?transaction=ADMIN&restriction=other&action=read&action=delete", nil)
			req.Header.Set("Authorization", "Bearer caller-token")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.status || rec.Body.String() != tc.body {
				t.Fatalf("response changed: %d %q", rec.Code, rec.Body.String())
			}
		})
	}
}
