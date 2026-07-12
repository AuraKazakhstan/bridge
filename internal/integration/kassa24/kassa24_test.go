// Дублирующие тесты Kassa24: фиксируют поведение PHP-моста 1:1
// (легаси-контракт с платёжным провайдером). Если тест падает — значит
// контракт изменился, и это надо согласовывать с Kassa24, а не «чинить» тест.
package kassa24

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yesmukhamedov/bridge/internal/auth"
)

const testStaticToken = "test-static-token-123"

// testEnv поднимает мок auth-server и мок core, возвращает готовый Handler.
type testEnv struct {
	kassa24    *Handler
	tokenCalls *atomic.Int32 // сколько раз мост сбегал за токеном
	coreSeen   *atomic.Value // последний запрос, дошедший до core: "METHOD path?query|auth|body"
	coreStatus *atomic.Int32 // статус, которым отвечает мок core
	coreBody   *atomic.Value // тело, которым отвечает мок core
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	env := &testEnv{
		tokenCalls: &atomic.Int32{},
		coreSeen:   &atomic.Value{},
		coreStatus: &atomic.Int32{},
		coreBody:   &atomic.Value{},
	}
	env.coreStatus.Store(int32(200))
	env.coreBody.Store(`{"ok":true}`)

	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := env.tokenCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// каждый раз новый токен — тест на отсутствие кэша это увидит
		_, _ = w.Write([]byte(`{"access_token":"tok-` + string(rune('0'+n)) + `"}`))
	}))
	t.Cleanup(authSrv.Close)

	coreSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		env.coreSeen.Store(r.Method + " " + r.URL.Path + "?" + r.URL.RawQuery +
			"|" + r.Header.Get("Authorization") + "|" + string(body))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(env.coreStatus.Load()))
		_, _ = w.Write([]byte(env.coreBody.Load().(string)))
	}))
	t.Cleanup(coreSrv.Close)

	a := auth.New(authSrv.URL, "WERP", "secret", "svc-user", "svc-pass", 5*time.Second)
	env.kassa24 = New(coreSrv.URL, testStaticToken, a, 5*time.Second)
	return env
}

func do(t *testing.T, h http.HandlerFunc, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// --- контракт: неверный/отсутствующий access_token ---

func TestCreatePayment_InvalidToken_Returns200WithLegacyBody(t *testing.T) {
	env := newTestEnv(t)

	rec := do(t, env.kassa24.CreatePayment, "POST", "/payments?access_token=WRONG", `{"contractNumber":"C-1"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("статус: получили %d, легаси-контракт требует 200", rec.Code)
	}
	if rec.Body.String() != invalidTokenObject {
		t.Fatalf("тело: %s", rec.Body.String())
	}
	if env.tokenCalls.Load() != 0 {
		t.Fatalf("мост не должен бегать за токеном при неверном access_token")
	}
	if env.coreSeen.Load() != nil {
		t.Fatalf("запрос не должен дойти до core")
	}
}

func TestProxyGet_InvalidToken_Returns200WithLegacyString(t *testing.T) {
	env := newTestEnv(t)
	h := env.kassa24.ProxyGet("/api/core/finance/kassa24/company")

	rec := do(t, h, "GET", "/company", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("статус: %d, ожидали 200", rec.Code)
	}
	// PHP отдавал JSON-строку с ЛИТЕРАЛЬНЫМИ \n и \" внутри
	var got string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("тело должно быть JSON-строкой: %v", err)
	}
	if got != invalidTokenString {
		t.Fatalf("легаси-строка изменилась:\n got: %q\nwant: %q", got, invalidTokenString)
	}
}

// --- контракт: createPayment ---

func TestCreatePayment_BuildsPaymentFormWithNullsForMissingFields(t *testing.T) {
	env := newTestEnv(t)

	rec := do(t, env.kassa24.CreatePayment, "POST",
		"/payments?access_token="+testStaticToken,
		`{"contractNumber":"C-42","paid":1500.50,"date":"2026-07-11","paymentId":"P1","rrn":"R1","terminal":"T1"}`)

	if rec.Code != http.StatusOK || rec.Body.String() != `{"message":"Form successfully handled"}` {
		t.Fatalf("ответ партнёру: %d %s", rec.Code, rec.Body.String())
	}

	seen := env.coreSeen.Load().(string)
	parts := strings.SplitN(seen, "|", 3)
	if !strings.HasPrefix(parts[0], "POST /api/core/finance/kassa24?") {
		t.Fatalf("апстрим-запрос: %s", parts[0])
	}
	if !strings.HasPrefix(parts[1], "Bearer tok-") {
		t.Fatalf("нет сервисного Bearer: %s", parts[1])
	}

	var form map[string]any
	if err := json.Unmarshal([]byte(parts[2]), &form); err != nil {
		t.Fatalf("тело на апстрим не JSON: %v", err)
	}
	if form["contractNumber"] != "C-42" {
		t.Fatalf("contractNumber: %v", form["contractNumber"])
	}
	if form["paid"] != 1500.50 {
		t.Fatalf("paid: %v", form["paid"])
	}
	// bankAccount не передан -> должен уйти null (как в PHP `?? null`)
	if v, ok := form["bankAccount"]; !ok || v != nil {
		t.Fatalf("bankAccount должен присутствовать и быть null, получили: %v (present=%v)", v, ok)
	}
	if len(form) != len(paymentFormFields) {
		t.Fatalf("на апстрим должны уходить ровно поля PaymentForm, получили: %v", form)
	}
}

func TestCreatePayment_UpstreamFailure_StillReturns200(t *testing.T) {
	env := newTestEnv(t)
	env.coreStatus.Store(int32(500))
	env.coreBody.Store(`{"boom":true}`)

	rec := do(t, env.kassa24.CreatePayment, "POST",
		"/payments?access_token="+testStaticToken, `{"contractNumber":"C-1"}`)

	// легаси: ответ send() игнорировался — партнёр всегда видит успех
	if rec.Code != http.StatusOK || rec.Body.String() != `{"message":"Form successfully handled"}` {
		t.Fatalf("ответ должен остаться 200 success (легаси), получили: %d %s", rec.Code, rec.Body.String())
	}
}

// --- контракт: GET-справки ---

func TestProxyGet_ForwardsOnlyDeclaredQueryParams(t *testing.T) {
	env := newTestEnv(t)
	h := env.kassa24.ProxyGet("/api/core/finance/kassa24/contract", "bukrsId", "customerId")

	do(t, h, "GET", "/contract?access_token="+testStaticToken+"&bukrsId=1000&customerId=77&hack=1&extra=x", "")

	seen := env.coreSeen.Load().(string)
	reqLine := strings.SplitN(seen, "|", 2)[0]
	if !strings.Contains(reqLine, "bukrsId=1000") || !strings.Contains(reqLine, "customerId=77") {
		t.Fatalf("объявленные параметры должны дойти: %s", reqLine)
	}
	for _, banned := range []string{"hack", "extra", "access_token"} {
		if strings.Contains(reqLine, banned) {
			t.Fatalf("параметр %q не должен уходить на апстрим: %s", banned, reqLine)
		}
	}
}

func TestProxyGet_UpstreamError_Returns404WithUpstreamBody(t *testing.T) {
	env := newTestEnv(t)
	env.coreStatus.Store(int32(401))
	env.coreBody.Store(`{"error":"invalid_token","error_description":"upstream says no"}`)
	h := env.kassa24.ProxyGet("/api/core/finance/kassa24/company")

	rec := do(t, h, "GET", "/company?access_token="+testStaticToken, "")

	// легаси-контракт: ЛЮБАЯ ошибка апстрима -> 404 с её телом
	if rec.Code != http.StatusNotFound {
		t.Fatalf("статус: %d, легаси требует 404", rec.Code)
	}
	if rec.Body.String() != `{"error":"invalid_token","error_description":"upstream says no"}` {
		t.Fatalf("тело апстрима должно пройти как есть: %s", rec.Body.String())
	}
}

func TestProxyGet_UpstreamErrorNonJSON_Returns404Null(t *testing.T) {
	env := newTestEnv(t)
	env.coreStatus.Store(int32(500))
	env.coreBody.Store(`<html>Internal Server Error</html>`)
	h := env.kassa24.ProxyGet("/api/core/finance/kassa24/company")

	rec := do(t, h, "GET", "/company?access_token="+testStaticToken, "")

	// PHP json_decode(не-JSON) -> null -> response()->json(null) -> "null"
	if rec.Code != http.StatusNotFound || rec.Body.String() != "null" {
		t.Fatalf("ожидали 404 null, получили: %d %s", rec.Code, rec.Body.String())
	}
}

func TestProxyGet_Success_PassesUpstreamBodyThrough(t *testing.T) {
	env := newTestEnv(t)
	env.coreBody.Store(`{"companies":[{"id":1,"name":"Aura"}]}`)
	h := env.kassa24.ProxyGet("/api/core/finance/kassa24/company")

	rec := do(t, h, "GET", "/company?access_token="+testStaticToken, "")

	if rec.Code != http.StatusOK || rec.Body.String() != `{"companies":[{"id":1,"name":"Aura"}]}` {
		t.Fatalf("тело апстрима должно пройти без изменений: %d %s", rec.Code, rec.Body.String())
	}
}

// --- решение пользователя: токен на каждый запрос, БЕЗ кэша ---

func TestToken_FetchedFreshOnEveryRequest_NeverCached(t *testing.T) {
	env := newTestEnv(t)
	h := env.kassa24.ProxyGet("/api/core/finance/kassa24/company")

	do(t, h, "GET", "/company?access_token="+testStaticToken, "")
	do(t, h, "GET", "/company?access_token="+testStaticToken, "")
	do(t, env.kassa24.CreatePayment, "POST", "/payments?access_token="+testStaticToken, `{"contractNumber":"C-1"}`)

	if got := env.tokenCalls.Load(); got != 3 {
		t.Fatalf("3 запроса = 3 похода за токеном (кэш запрещён), получили: %d", got)
	}
}

func TestStaticTokenEmpty_RejectsEverything(t *testing.T) {
	env := newTestEnv(t)
	env.kassa24.staticToken = "" // не сконфигурирован -> всё отвергается
	h := env.kassa24.ProxyGet("/api/core/finance/kassa24/company")

	rec := do(t, h, "GET", "/company?access_token=", "")

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "invalid_token") {
		t.Fatalf("пустой сконфигурированный токен не должен пускать никого: %d %s", rec.Code, rec.Body.String())
	}
	if env.coreSeen.Load() != nil {
		t.Fatalf("запрос не должен дойти до core")
	}
}
