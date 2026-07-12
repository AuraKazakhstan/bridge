// Дублирующие тесты delivery-репортов SMS-провайдера (/info-tech-status).
package infotech

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

func newAuthMock(t *testing.T) *auth.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"svc-token"}`))
	}))
	t.Cleanup(srv.Close)
	return auth.New(srv.URL, "WERP", "secret", "svc", "pw", 5*time.Second)
}

func TestDeliveryReport_MapsSnakeCaseToCamelCaseAndEchoesForm(t *testing.T) {
	seen := &atomic.Value{}
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen.Store(r.URL.Path + "|" + r.Header.Get("Authorization") + "|" + string(body))
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(core.Close)
	h := New(core.URL, newAuthMock(t), 5*time.Second)

	req := httptest.NewRequest("GET",
		"/info-tech-status?bulk_id=B1&message_id=M1&to=77011234567&status=DELIVERED&sms_count=2", nil)
	rec := httptest.NewRecorder()
	h.DeliveryReport(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("статус: %d", rec.Code)
	}
	// ответ провайдеру — эхо формы в camelCase
	var echo map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &echo); err != nil {
		t.Fatalf("эхо не JSON: %v", err)
	}
	if echo["bulkId"] != "B1" || echo["messageId"] != "M1" || echo["to"] != "77011234567" ||
		echo["status"] != "DELIVERED" || echo["smsCount"] != "2" {
		t.Fatalf("переданные поля: %v", echo)
	}
	// отсутствующие поля -> легаси-заглушки default_<имя>
	if echo["sender"] != "default_sender" || echo["mnc"] != "default_mnc" || echo["err"] != "default_err" ||
		echo["extraId"] != "default_extra_id" || echo["sentAt"] != "default_sent_at" || echo["doneAt"] != "default_done_at" {
		t.Fatalf("дефолты: %v", echo)
	}

	// в core ушла ровно эта же форма под сервисным Bearer
	parts := strings.SplitN(seen.Load().(string), "|", 3)
	if parts[0] != "/api/core/sms-info-tech/create-report" || parts[1] != "Bearer svc-token" {
		t.Fatalf("апстрим: %s %s", parts[0], parts[1])
	}
	if parts[2] != rec.Body.String() {
		t.Fatalf("в core должна уйти та же форма, что и в эхо:\ncore: %s\necho: %s", parts[2], rec.Body.String())
	}
}

func TestDeliveryReport_CoreFailure_StillReturns200WithForm(t *testing.T) {
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	}))
	t.Cleanup(core.Close)
	h := New(core.URL, newAuthMock(t), 5*time.Second)

	req := httptest.NewRequest("GET", "/info-tech-status?status=FAILED", nil)
	rec := httptest.NewRecorder()
	h.DeliveryReport(rec, req)

	// легаси: провайдер всегда видит 200 с эхом формы
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"FAILED"`) {
		t.Fatalf("ответ: %d %s", rec.Code, rec.Body.String())
	}
}
