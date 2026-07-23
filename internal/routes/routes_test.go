package routes

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yesmukhamedov/bridge/internal/config"
)

// newEnv поднимает мок-апстримы и собирает полный роутер.
// Мок отвечает 200 и запоминает последний "METHOD path" на каждый хост.
func newEnv(t *testing.T) (http.Handler, map[string]*atomic.Value) {
	t.Helper()
	seen := map[string]*atomic.Value{}
	up := func(name string) string {
		v := &atomic.Value{}
		seen[name] = v
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			v.Store(r.Method + " " + r.URL.Path)
			if r.URL.Path == "/oauth/token" {
				_, _ = w.Write([]byte(`{"access_token":"tok"}`))
				return
			}
			_, _ = w.Write([]byte(`{}`))
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}

	cfg := &config.Config{
		AuthURL: up("auth"), CoreURL: up("core"),
		ServiceURL: up("service"), CrmURL: up("crm"),
		PkbURL: up("pkb"), TwilioURL: up("twilio"),
		TwilioReceiveForwardURL: up("receiver"),
		OAuthClientID:           "WERP", OAuthClientSecret: "s",
		ServiceUsername: "svc", ServicePassword: "pw",
		Kassa24StaticToken: "k24-token",
		UpstreamTimeout:    5 * time.Second,
		RateLimitPerSecond: 1000, RateLimitBurst: 1000,
	}
	return New(cfg), seen
}

func do(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestRoutes_MobileAllowlist(t *testing.T) {
	h, seen := newEnv(t)

	// задекларированные мобильные пути проксируются 1:1
	for _, tc := range []struct{ method, path, upstream string }{
		{"GET", "/api/service/smcs/getServiceList", "service"},
		{"POST", "/api/service/smcs/master/draft/create-and-payment", "service"},
		{"GET", "/api/service/smcs/master/draft/17", "service"},
		{"GET", "/api/service/smecam/5", "service"},
		{"PUT", "/api/service/smecam/edit", "service"},
		{"GET", "/api/core/logistics/accountabilities/staff-matnrs/9", "core"},
		{"DELETE", "/api/core/logistics/accountabilities/3", "core"},
		{"GET", "/api/core/finance/mainoperation/fetchCashBankHkontsByBranch", "core"},
	} {
		rec := do(h, tc.method, tc.path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: %d", tc.method, tc.path, rec.Code)
		}
		if got := seen[tc.upstream].Load(); got != tc.method+" "+tc.path {
			t.Fatalf("%s %s ушёл не туда: %v", tc.method, tc.path, got)
		}
	}

	// НЕзадекларированные пути core/service — 404, на апстрим не уходят
	// (в PHP был явный список; сквозной префикс открывал бы всё)
	for _, path := range []string{
		"/api/core/finance/kassa24/get-payments", // внутренний, наружу нельзя
		"/api/service/smcs/create_and_accept_payment",
		"/api/core/hr/staff/user-demopad",
	} {
		before := seen["core"].Load()
		if rec := do(h, "GET", path); rec.Code != http.StatusNotFound {
			t.Fatalf("%s должен быть 404 (вне allowlist): %d", path, rec.Code)
		}
		if after := seen["core"].Load(); after != before {
			t.Fatalf("%s не должен доходить до апстрима", path)
		}
	}
}

func TestRoutes_LegacyTokenWired(t *testing.T) {
	h, seen := newEnv(t)

	rec := do(h, "POST", "/token?username=u&password=p")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "tok") {
		t.Fatalf("легаси /token: %d %s", rec.Code, rec.Body.String())
	}
	if got := seen["auth"].Load(); got != "POST /oauth/token" {
		t.Fatalf("должен уходить в auth-server: %v", got)
	}

	rec = do(h, "POST", "/token/mobile")
	if got := seen["auth"].Load(); got != "POST /oauth/token" || rec.Code != http.StatusOK {
		t.Fatalf("/token/mobile: %d %v", rec.Code, got)
	}
}

func TestRoutes_StaffDispatchAndAllowlist404(t *testing.T) {
	h, seen := newEnv(t)

	if rec := do(h, "GET", "/requests/staff/42"); rec.Code != http.StatusOK {
		t.Fatalf("staff-ветка: %d", rec.Code)
	}
	if got := seen["core"].Load(); got != "GET /api/core/marketing/aitu/requests/staff/42" {
		t.Fatalf("staff-переписывание: %v", got)
	}

	if rec := do(h, "GET", "/requests/123/unknown"); rec.Code != http.StatusNotFound {
		t.Fatalf("/requests/123/unknown: %d, ожидался 404", rec.Code)
	}
	if rec := do(h, "GET", "/etc/passwd"); rec.Code != http.StatusNotFound {
		t.Fatalf("незадекларированный путь: %d, ожидался 404", rec.Code)
	}
}

// captureBody — next-хендлер, запоминающий тело и Content-Type, что дошли бы
// до апстрима после phpBodyCompat.
func captureBody(gotCT *string, gotBody *[]byte) http.Handler {
	return http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*gotBody = b
		*gotCT = r.Header.Get("Content-Type")
	})
}

func TestPHPBodyCompat(t *testing.T) {
	// Проверяем, что до апстрима доходит JSON-тело, собранное из query+тело,
	// как это делал PHP $request->all() (см. phpBodyCompat).
	run := func(method, target, ct, body string) (string, map[string]any) {
		var gotCT string
		var gotBody []byte
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		phpBodyCompat(captureBody(&gotCT, &gotBody)).ServeHTTP(httptest.NewRecorder(), req)
		var m map[string]any
		if len(gotBody) > 0 {
			_ = json.Unmarshal(gotBody, &m)
		}
		return gotCT, m
	}

	// 1) code в query, тела нет -> уходит JSON-тело с code (главный баг OTP)
	ct, m := run("POST", "/credit-rating/check-consent-code?code=1234&requestId=5", "", "")
	if !strings.Contains(ct, "json") || m["code"] != "1234" || m["requestId"] != "5" {
		t.Fatalf("query-only: ct=%q body=%v", ct, m)
	}

	// 2) form-urlencoded -> JSON-тело
	ct, m = run("POST", "/credit-rating/check-consent-code", "application/x-www-form-urlencoded", "code=1234&requestId=5")
	if !strings.Contains(ct, "json") || m["code"] != "1234" {
		t.Fatalf("form: ct=%q body=%v", ct, m)
	}

	// 3) JSON-тело + query -> объединяются, тело приоритетнее
	ct, m = run("POST", "/credit-rating/check-consent-code?requestId=9&extra=q", "application/json", `{"code":"1234","requestId":"5"}`)
	if m["code"] != "1234" || m["requestId"] != "5" || m["extra"] != "q" {
		t.Fatalf("json+query merge: ct=%q body=%v", ct, m)
	}

	// 4) пустой запрос -> тела нет, Content-Type не выставляется
	ct, body := func() (string, []byte) {
		var gotCT string
		var gotBody []byte
		req := httptest.NewRequest("POST", "/requests/es/callback", nil)
		phpBodyCompat(captureBody(&gotCT, &gotBody)).ServeHTTP(httptest.NewRecorder(), req)
		return gotCT, gotBody
	}()
	if ct != "" || len(bytes.TrimSpace(body)) != 0 {
		t.Fatalf("empty: ct=%q body=%q", ct, body)
	}

	// 5) неизвестный тип тела -> пробрасывается как есть
	ct, gotRaw := func() (string, []byte) {
		var gotCT string
		var gotBody []byte
		req := httptest.NewRequest("POST", "/requests", strings.NewReader("raw-payload"))
		req.Header.Set("Content-Type", "text/plain")
		phpBodyCompat(captureBody(&gotCT, &gotBody)).ServeHTTP(httptest.NewRecorder(), req)
		return gotCT, gotBody
	}()
	if ct != "text/plain" || string(gotRaw) != "raw-payload" {
		t.Fatalf("passthrough: ct=%q body=%q", ct, gotRaw)
	}
}
