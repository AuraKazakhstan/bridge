// Дублирующие тесты ПКБ: фиксируют поведение PHP FCBController 1:1.
package pkb

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type pkbEnv struct {
	handler *Handler

	mu       sync.Mutex
	requests []string // "PATH|auth|consent" в порядке поступления

	availabilityStatus int
	availabilityBody   string
	scorStatus         int
	scorBody           string
}

func newPkbEnv(t *testing.T) *pkbEnv {
	t.Helper()
	env := &pkbEnv{
		availabilityStatus: 200, availabilityBody: `{"available":true}`,
		scorStatus: 200, scorBody: `{"score":650,"grade":"B"}`,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.mu.Lock()
		env.requests = append(env.requests,
			r.URL.Path+"|"+r.Header.Get("Authorization")+"|"+r.Header.Get("Consent-Confirmed"))
		env.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if len(r.URL.Path) > 13 && r.URL.Path[:13] == "/availability" {
			w.WriteHeader(env.availabilityStatus)
			_, _ = w.Write([]byte(env.availabilityBody))
			return
		}
		w.WriteHeader(env.scorStatus)
		_, _ = w.Write([]byte(env.scorBody))
	}))
	t.Cleanup(srv.Close)

	env.handler = New(srv.URL, 5*time.Second)
	return env
}

func (env *pkbEnv) call(t *testing.T, iin, base64 string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/credit-rating/iin/"+iin, nil)
	req.SetPathValue("iin", iin)
	if base64 != "" {
		req.Header.Set("base64", base64)
	}
	rec := httptest.NewRecorder()
	env.handler.CreditRatingByIIN(rec, req)
	return rec
}

func TestPKB_MissingBase64_Returns400ExactBody(t *testing.T) {
	env := newPkbEnv(t)

	rec := env.call(t, "880101300123", "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("статус: %d", rec.Code)
	}
	if rec.Body.String() != `{"error":"\"base64\" is required"}` {
		t.Fatalf("точное легаси-тело изменилось: %s", rec.Body.String())
	}
	if len(env.requests) != 0 {
		t.Fatalf("без base64 к ПКБ ходить нельзя")
	}
}

func TestPKB_HappyPath_TwoStepCallWithProperHeaders(t *testing.T) {
	env := newPkbEnv(t)

	rec := env.call(t, "880101300123", "Q1JFRDpQQVNT")

	if rec.Code != http.StatusOK || rec.Body.String() != `{"score":650,"grade":"B"}` {
		t.Fatalf("ответ: %d %s (ожидали тело scor как есть)", rec.Code, rec.Body.String())
	}
	if len(env.requests) != 2 {
		t.Fatalf("должно быть ровно 2 шага, было: %v", env.requests)
	}
	if env.requests[0] != "/availability/880101300123|Basic Q1JFRDpQQVNT|" {
		t.Fatalf("шаг 1 (availability, Basic, БЕЗ consent): %s", env.requests[0])
	}
	if env.requests[1] != "/scor/880101300123|Basic Q1JFRDpQQVNT|1" {
		t.Fatalf("шаг 2 (scor, Basic, Consent-Confirmed: 1): %s", env.requests[1])
	}
}

func TestPKB_AvailabilityFails_Returns200WithIinMergedAndSkipsScor(t *testing.T) {
	env := newPkbEnv(t)
	env.availabilityStatus = 400
	env.availabilityBody = `{"code":"SUBJECT_NOT_FOUND","message":"нет данных"}`

	rec := env.call(t, "990202400456", "AUTH")

	// легаси: ошибка ПКБ -> 200 с iin + полями ошибки
	if rec.Code != http.StatusOK {
		t.Fatalf("легаси-контракт требует 200 при ошибке ПКБ, получили: %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело: %v", err)
	}
	if body["iin"] != "990202400456" || body["code"] != "SUBJECT_NOT_FOUND" || body["message"] != "нет данных" {
		t.Fatalf("iin + поля ошибки: %v", body)
	}
	if len(env.requests) != 1 {
		t.Fatalf("после падения availability шаг scor не должен выполняться: %v", env.requests)
	}
}

func TestPKB_ScorFails_Returns200WithIinMerged(t *testing.T) {
	env := newPkbEnv(t)
	env.scorStatus = 403
	env.scorBody = `{"error":"consent_required"}`

	rec := env.call(t, "770303500789", "AUTH")

	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusOK || body["iin"] != "770303500789" || body["error"] != "consent_required" {
		t.Fatalf("ответ: %d %v", rec.Code, body)
	}
}

func TestPKB_ErrorBodyNotJSON_Returns200WithIinOnly(t *testing.T) {
	env := newPkbEnv(t)
	env.availabilityStatus = 500
	env.availabilityBody = `<html>gateway timeout</html>`

	rec := env.call(t, "660404600111", "AUTH")

	if rec.Code != http.StatusOK || rec.Body.String() != `{"iin":"660404600111"}` {
		t.Fatalf("не-JSON ошибка ПКБ -> только iin: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPKB_ErrorBodyWithIinField_UpstreamIinWins(t *testing.T) {
	env := newPkbEnv(t)
	env.availabilityStatus = 400
	env.availabilityBody = `{"iin":"UPSTREAM-IIN","code":"X"}`

	rec := env.call(t, "550505700222", "AUTH")

	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	// array_merge в PHP: значение из тела ошибки перекрывает наш iin
	if body["iin"] != "UPSTREAM-IIN" {
		t.Fatalf("поле iin из ответа ПКБ должно побеждать (array_merge): %v", body)
	}
}
