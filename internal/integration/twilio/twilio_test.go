// Дублирующие тесты Twilio: исходящие send/messages (креды передаёт
// вызывающий — werp_crm, решение 2026-07-11) и входящий вебхук receive.
package twilio

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yesmukhamedov/bridge/internal/auth"
)

const (
	testSID   = "ACtest1234567890"
	testToken = "secret-token"
)

func basicAuth() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(testSID+":"+testToken))
}

func newAuthMock(t *testing.T) (*auth.Client, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"svc-token"}`))
	}))
	t.Cleanup(srv.Close)
	return auth.New(srv.URL, "WERP", "secret", "svc", "pw", 5*time.Second), calls
}

type outEnv struct {
	tw     *Handler
	seen   *atomic.Value // "METHOD path?query|auth|body"
	status *atomic.Int32
	body   *atomic.Value
}

func newOutEnv(t *testing.T) *outEnv {
	t.Helper()
	env := &outEnv{seen: &atomic.Value{}, status: &atomic.Int32{}, body: &atomic.Value{}}
	env.status.Store(200)
	env.body.Store(`{"sid":"SM999","from":"whatsapp:+77755702449","to":"whatsapp:+77011234567","direction":"outbound-api","body":"hi"}`)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		env.seen.Store(r.Method + " " + r.URL.Path + "?" + r.URL.RawQuery + "|" + r.Header.Get("Authorization") + "|" + string(b))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(env.status.Load()))
		_, _ = w.Write([]byte(env.body.Load().(string)))
	}))
	t.Cleanup(api.Close)

	authClient, _ := newAuthMock(t)
	env.tw = New(api.URL, "", authClient, 5*time.Second)
	return env
}

// --- send ---

func TestSend_NoAuth_Returns400(t *testing.T) {
	env := newOutEnv(t)

	req := httptest.NewRequest("POST", "/twilio/send", strings.NewReader(`{"from":"1","to":"2"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	env.tw.Send(rec, req)

	if rec.Code != http.StatusBadRequest || env.seen.Load() != nil {
		t.Fatalf("без Basic-кредов запрос не должен уйти в Twilio: %d", rec.Code)
	}
}

func TestSend_BuildsTwilioRequestAndMapsResponse(t *testing.T) {
	env := newOutEnv(t)

	payload := `{"from":"77755702449","to":"77011234567","contentSid":"HXc41e49","contentVariables":{"1":"Алматы","2":"Roboclean"}}`
	req := httptest.NewRequest("POST", "/twilio/send", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", basicAuth())
	rec := httptest.NewRecorder()
	env.tw.Send(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("статус: %d %s", rec.Code, rec.Body.String())
	}

	seen := env.seen.Load().(string)
	parts := strings.SplitN(seen, "|", 3)
	// SID из кредов вызывающего попадает в URL, Basic пробрасывается как есть
	if parts[0] != "POST /2010-04-01/Accounts/"+testSID+"/Messages.json?" {
		t.Fatalf("URL Twilio: %s", parts[0])
	}
	if parts[1] != basicAuth() {
		t.Fatalf("Basic вызывающего должен пробрасываться: %s", parts[1])
	}
	form, _ := url.ParseQuery(parts[2])
	if form.Get("From") != "whatsapp:+77755702449" || form.Get("To") != "whatsapp:+77011234567" {
		t.Fatalf("whatsapp:+ префиксы: %v", form)
	}
	if form.Get("ContentSid") != "HXc41e49" {
		t.Fatalf("ContentSid: %v", form)
	}
	var cv map[string]string
	if err := json.Unmarshal([]byte(form.Get("ContentVariables")), &cv); err != nil || cv["1"] != "Алматы" {
		t.Fatalf("ContentVariables должны json-кодироваться: %v (%v)", form.Get("ContentVariables"), err)
	}
	if form.Has("Body") {
		t.Fatalf("пустой body не должен отправляться: %v", form)
	}

	// ответ: responseForm PHP — from/to без whatsapp:+, direction, body, sid
	var out map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["from"] != "77755702449" || out["to"] != "77011234567" ||
		out["direction"] != "outbound-api" || out["sid"] != "SM999" || out["body"] != "hi" {
		t.Fatalf("responseForm: %v", out)
	}
}

func TestSend_EmptyContentVariablesNotSent(t *testing.T) {
	env := newOutEnv(t)

	req := httptest.NewRequest("POST", "/twilio/send",
		strings.NewReader(`{"from":"1","to":"2","contentVariables":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", basicAuth())
	rec := httptest.NewRecorder()
	env.tw.Send(rec, req)

	form, _ := url.ParseQuery(strings.SplitN(env.seen.Load().(string), "|", 3)[2])
	// PHP empty({}) -> true: пустые contentVariables не уходили в Twilio
	if form.Has("ContentVariables") {
		t.Fatalf("пустые contentVariables не должны отправляться: %v", form)
	}
}

func TestSend_TwilioRejects_Returns502WithTwilioBody(t *testing.T) {
	env := newOutEnv(t)
	env.status.Store(400)
	env.body.Store(`{"code":63016,"message":"template not approved"}`)

	req := httptest.NewRequest("POST", "/twilio/send", strings.NewReader(`{"from":"1","to":"2"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", basicAuth())
	rec := httptest.NewRecorder()
	env.tw.Send(rec, req)

	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "63016") {
		t.Fatalf("ошибка Twilio -> 502 с её телом (Feign в werp_crm увидит не-2xx): %d %s", rec.Code, rec.Body.String())
	}
}

// --- messages ---

func TestListMessages_MapsFiltersAndWrapsSuccess(t *testing.T) {
	env := newOutEnv(t)
	env.body.Store(`{"messages":[{"sid":"SM1"}]}`)

	req := httptest.NewRequest("GET",
		"/twilio/messages?dateFrom=2026-07-01&dateTo=2026-07-11&from=777&pageSize=20", nil)
	req.Header.Set("Authorization", basicAuth())
	rec := httptest.NewRecorder()
	env.tw.ListMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("легаси: всегда 200, получили %d", rec.Code)
	}
	seen := env.seen.Load().(string)
	reqLine := strings.SplitN(seen, "|", 2)[0]
	for _, want := range []string{"DateSent%3E=2026-07-01", "DateSent%3C=2026-07-11", "From=whatsapp%3A%2B777", "PageSize=20"} {
		if !strings.Contains(reqLine, want) {
			t.Fatalf("нет %s в запросе к Twilio: %s", want, reqLine)
		}
	}

	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["status"] != "success" || out["message"] != "Message sent successfully!" {
		t.Fatalf("обёртка успеха: %v", out)
	}
	if _, ok := out["data"].(map[string]any); !ok {
		t.Fatalf("data должен содержать ответ Twilio: %v", out)
	}
}

func TestListMessages_UrlParamReplacesQuery(t *testing.T) {
	env := newOutEnv(t)

	req := httptest.NewRequest("GET",
		"/twilio/messages?from=777&url="+
			"PageToken%3DPA123%26Page%3D2", nil)
	req.Header.Set("Authorization", basicAuth())
	rec := httptest.NewRecorder()
	env.tw.ListMessages(rec, req)

	reqLine := strings.SplitN(env.seen.Load().(string), "|", 2)[0]
	// легаси: url заменяет собой ВСЮ query (пагинация Twilio)
	if !strings.Contains(reqLine, "PageToken=PA123") || strings.Contains(reqLine, "From=") {
		t.Fatalf("url-параметр должен заменять query целиком: %s", reqLine)
	}
	_ = rec
}

func TestListMessages_TwilioError_Still200WithErrorWrap(t *testing.T) {
	env := newOutEnv(t)
	env.status.Store(401)
	env.body.Store(`{"code":20003,"message":"Authenticate"}`)

	req := httptest.NewRequest("GET", "/twilio/messages", nil)
	req.Header.Set("Authorization", basicAuth())
	rec := httptest.NewRecorder()
	env.tw.ListMessages(rec, req)

	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	// легаси-контракт PHP: ошибка тоже HTTP 200, но с обёрткой error
	if rec.Code != http.StatusOK || out["status"] != "error" || out["message"] != "Failed to send message." {
		t.Fatalf("обёртка ошибки при HTTP 200: %d %v", rec.Code, out)
	}
}

// --- receive ---

func TestReceive_ForwardsWebhookDataWithServiceToken(t *testing.T) {
	authClient, tokenCalls := newAuthMock(t)
	seen := &atomic.Value{}
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen.Store(r.URL.Path + "|" + r.Header.Get("Authorization") + "|" + string(body))
		w.WriteHeader(200)
	}))
	t.Cleanup(receiver.Close)
	tw := New("https://api.twilio.com", receiver.URL+"/api/messages/receive", authClient, 5*time.Second)

	form := "MessageSid=SM123&From=whatsapp%3A%2B77011234567&Body=hello"
	req := httptest.NewRequest("POST", "/twilio/receive", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	tw.Receive(rec, req)

	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("успех = 200 без тела: %d %q", rec.Code, rec.Body.String())
	}

	parts := strings.SplitN(seen.Load().(string), "|", 3)
	if parts[0] != "/api/messages/receive" || parts[1] != "Bearer svc-token" {
		t.Fatalf("приёмник: %s %s", parts[0], parts[1])
	}
	// фикс PHP-бага: уходят реальные данные вебхука, а не "{}"
	var payload map[string]any
	if err := json.Unmarshal([]byte(parts[2]), &payload); err != nil {
		t.Fatalf("тело: %v", err)
	}
	if payload["MessageSid"] != "SM123" || payload["From"] != "whatsapp:+77011234567" || payload["Body"] != "hello" {
		t.Fatalf("данные вебхука должны дойти до приёмника: %v", payload)
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("один запрос = один поход за токеном: %d", tokenCalls.Load())
	}
}

func TestReceive_ReceiverDown_Returns502SoTwilioRetries(t *testing.T) {
	authClient, _ := newAuthMock(t)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	}))
	t.Cleanup(receiver.Close)
	tw := New("https://api.twilio.com", receiver.URL, authClient, 5*time.Second)

	req := httptest.NewRequest("POST", "/twilio/receive", strings.NewReader("MessageSid=SM1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	tw.Receive(rec, req)

	// 502 -> Twilio перепошлёт вебхук, сообщение не потеряется молча
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("отказ приёмника должен давать 502: %d", rec.Code)
	}
}

func TestReceive_NotConfigured_Returns502(t *testing.T) {
	authClient, tokenCalls := newAuthMock(t)
	tw := New("https://api.twilio.com", "", authClient, 5*time.Second)

	req := httptest.NewRequest("POST", "/twilio/receive", nil)
	rec := httptest.NewRecorder()
	tw.Receive(rec, req)

	if rec.Code != http.StatusBadGateway || tokenCalls.Load() != 0 {
		t.Fatalf("без конфигурации приёмника: 502 и никаких походов за токеном (%d, calls=%d)", rec.Code, tokenCalls.Load())
	}
}
