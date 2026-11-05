package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLegacyToken_RefreshForwardsOnlyTokenWithServerBasic(t *testing.T) {
	for _, input := range []struct{ contentType, body string }{
		{"application/x-www-form-urlencoded", "grant_type=refresh_token&refresh_token=old-refresh&username=attacker&password=bad&client_id=evil&client_secret=evil"},
		{"application/json", `{"grant_type":"refresh_token","refresh_token":"old-refresh","username":"attacker","password":"bad","client_id":"evil","client_secret":"evil"}`},
	} {
		t.Run(input.contentType, func(t *testing.T) {
			body := `{"access_token":"new","refresh_token":"rotated","expires_in":900,"extra":{"preserved":true}}`
			h, seen := newTokenEnv(t, http.StatusOK, body)
			r := httptest.NewRequest("POST", "/token", strings.NewReader(input.body))
			r.Header.Set("Content-Type", input.contentType)
			r.SetBasicAuth("untrusted", "untrusted")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusOK || w.Body.String() != body {
				t.Fatalf("response not preserved: %d %s", w.Code, w.Body.String())
			}
			if got := seen.Load(); got != "WERP:client-secret|grant_type=refresh_token&refresh_token=old-refresh" {
				t.Fatalf("unexpected upstream request: %v", got)
			}
		})
	}
}

func TestLegacyToken_ExplicitPasswordKeepsLegacyContract(t *testing.T) {
	for _, status := range []int{200, 400, 401, 503} {
		h, seen := newTokenEnv(t, status, `{"error":"secret"}`)
		r := httptest.NewRequest("POST", "/token", strings.NewReader("grant_type=password&username=x&password=y"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		wantStatus, wantBody := 200, `{"error":"secret"}`
		if status != 200 {
			wantStatus, wantBody = 500, `{"error":"Ошибка при получении токена"}`
		}
		if w.Code != wantStatus || w.Body.String() != wantBody {
			t.Fatalf("password contract: %d %s", w.Code, w.Body.String())
		}
		if seen.Load() != "WERP:client-secret|grant_type=password&password=y&username=x" {
			t.Fatalf("password grant changed: %v", seen.Load())
		}
	}
}

func TestLegacyToken_RefreshErrorsAreSanitized(t *testing.T) {
	for _, tc := range []struct {
		status     int
		body       string
		wantStatus int
		wantBody   string
	}{
		{400, `{"error":"invalid_grant","error_description":"secret refresh"}`, 400, `{"error":"invalid_grant"}`},
		{401, `{"error":"invalid_token","secret":"refresh"}`, 401, `{"error":"invalid_token"}`},
		{400, `{"error":"invalid_client","error_description":"secret client"}`, 400, `{"error":"invalid_client"}`},
		{401, `{"error":"unauthorized_client","error_description":"secret client"}`, 401, `{"error":"unauthorized_client"}`},
		{400, `{"error":"unsupported_grant_type","error_description":"secret"}`, 400, `{"error":"unsupported_grant_type"}`},
		{400, `{"error":"secret arbitrary code"}`, 400, `{"error":"server_error"}`},
		{401, `secret malformed body`, 401, `{"error":"server_error"}`},
		{400, `{"error":"invalid_grant","error":42}`, 400, `{"error":"server_error"}`},
		{503, `secret upstream body`, 503, `{"error":"temporarily_unavailable"}`},
		{500, `secret upstream body`, 502, `{"error":"server_error"}`},
	} {
		h, _ := newTokenEnv(t, tc.status, tc.body)
		r := httptest.NewRequest("POST", "/token", strings.NewReader("grant_type=refresh_token&refresh_token=old"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.wantStatus || w.Body.String() != tc.wantBody {
			t.Errorf("upstream %d: got %d %s", tc.status, w.Code, w.Body.String())
		}
	}
}

func TestLegacyToken_RefreshNetworkFailureIsGeneric(t *testing.T) {
	srv := httptest.NewServer(nil)
	srv.Close()
	h := NewLegacyTokenHandler(New(srv.URL, "client", "secret", "user", "password", time.Second))
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/token?grant_type=refresh_token&refresh_token=secret", nil)
	h.ServeHTTP(w, r)
	if w.Code != 503 || w.Body.String() != `{"error":"temporarily_unavailable"}` {
		t.Fatalf("network failure leaked: %d %s", w.Code, w.Body.String())
	}
}

func TestLegacyToken_InvalidGrantInputDoesNotCallUpstream(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"grant_type=client_credentials", "unsupported_grant_type"},
		{"grant_type=refresh_token", "invalid_request"},
		{"grant_type=refresh_token&refresh_token=", "invalid_request"},
		{"grant_type=refresh_token&refresh_token=+++", "invalid_request"},
	} {
		h, seen := newTokenEnv(t, 200, `{"access_token":"unexpected"}`)
		r := httptest.NewRequest("POST", "/token", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 || w.Body.String() != `{"error":"`+tc.want+`"}` {
			t.Errorf("validation failure: %d %s", w.Code, w.Body.String())
		}
		if seen.Load() != nil {
			t.Fatal("invalid input reached upstream")
		}
	}
}
