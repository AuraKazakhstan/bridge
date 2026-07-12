// Дублирующие тесты легаси POST /token (PHP AituRequestsController@token):
// мост добавляет Basic OAuth-клиента, любая ошибка -> 500 с легаси-телом.
package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTokenEnv(t *testing.T, status int, body string) (*LegacyTokenHandler, *atomic.Value) {
	t.Helper()
	seen := &atomic.Value{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		user, pass, _ := r.BasicAuth()
		seen.Store(user + ":" + pass + "|" + r.PostForm.Encode())
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewLegacyTokenHandler(New(srv.URL, "WERP", "client-secret", "svc", "pw", 5*time.Second)), seen
}

func TestLegacyToken_AddsClientBasicAndForwardsUserCreds(t *testing.T) {
	lt, seen := newTokenEnv(t, 200, `{"access_token":"abc","token_type":"bearer"}`)

	req := httptest.NewRequest("POST", "/token",
		strings.NewReader(`{"username":"aitu-user","password":"aitu-pass"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	lt.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("успех должен отдаваться 200: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["access_token"] != "abc" {
		t.Fatalf("тело auth-server должно отдаваться как есть: %s", rec.Body.String())
	}

	got := seen.Load().(string)
	// Basic клиента добавляет мост (в PHP был захардкожен WERP:password),
	// grant_type=password + креды вызывающего уходят формой
	if !strings.HasPrefix(got, "WERP:client-secret|") {
		t.Fatalf("Basic OAuth-клиента: %s", got)
	}
	for _, want := range []string{"grant_type=password", "username=aitu-user", "password=aitu-pass"} {
		if !strings.Contains(got, want) {
			t.Fatalf("нет %s в форме к auth-server: %s", want, got)
		}
	}
}

func TestLegacyToken_AuthErrorHiddenBehindLegacy500(t *testing.T) {
	lt, _ := newTokenEnv(t, 400, `{"error":"invalid_grant"}`)

	req := httptest.NewRequest("POST", "/token", strings.NewReader("username=x&password=y"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	lt.ServeHTTP(rec, req)

	// контракт PHP: любая ошибка -> 500 с этим телом, детали не раскрываются
	if rec.Code != http.StatusInternalServerError ||
		rec.Body.String() != `{"error":"Ошибка при получении токена"}` {
		t.Fatalf("легаси-ошибка: %d %s", rec.Code, rec.Body.String())
	}
}

func TestLegacyToken_AuthServerDown_Legacy500(t *testing.T) {
	srv := httptest.NewServer(nil)
	srv.Close() // умерший auth-server
	lt := NewLegacyTokenHandler(New(srv.URL, "WERP", "s", "u", "p", time.Second))

	req := httptest.NewRequest("POST", "/token?username=x&password=y", nil)
	rec := httptest.NewRecorder()
	lt.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError ||
		rec.Body.String() != `{"error":"Ошибка при получении токена"}` {
		t.Fatalf("недоступный auth-server -> легаси 500: %d %s", rec.Code, rec.Body.String())
	}
}
