// Дублирующие тесты форм: фиксируют маппинг PHP saveTildaForm/saveWebForm.
package target

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// formsEnv — мок CRM: справочники + захват сохранённой формы.
type formsEnv struct {
	forms      *Handler
	savedForm  *atomic.Value // JSON-тело, дошедшее до /api/crm2/target/save
	savedAuth  *atomic.Value // заголовок Authorization на save
	saveStatus *atomic.Int32
	locQuery   *atomic.Value // query, с которым запросили locationOptions
	refsDown   *atomic.Bool  // справочники отвечают 500
}

func newFormsEnv(t *testing.T) *formsEnv {
	t.Helper()
	env := &formsEnv{
		savedForm:  &atomic.Value{},
		savedAuth:  &atomic.Value{},
		saveStatus: &atomic.Int32{},
		locQuery:   &atomic.Value{},
		refsDown:   &atomic.Bool{},
	}
	env.saveStatus.Store(200)

	crm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if env.refsDown.Load() && strings.Contains(r.URL.Path, "Options") {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/crm2/target/businessAreaOptions":
			_, _ = w.Write([]byte(`[{"id":5,"name":"roboclean"},{"id":9,"name":"cebilon"}]`))
		case "/api/crm2/target/locationOptions":
			env.locQuery.Store(r.URL.RawQuery)
			_, _ = w.Write([]byte(`[{"id":12,"name":"Алматы"},{"id":15,"name":"Астана"}]`))
		case "/api/crm2/target/sourceAppealOptions":
			_, _ = w.Write([]byte(`[{"id":7,"name":"tilda"},{"id":11,"name":"instagram"}]`))
		case "/api/crm2/target/save":
			body, _ := io.ReadAll(r.Body)
			env.savedForm.Store(string(body))
			env.savedAuth.Store(r.Header.Get("Authorization"))
			w.WriteHeader(int(env.saveStatus.Load()))
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(crm.Close)

	env.forms = New(crm.URL, 5*time.Second)
	return env
}

func (env *formsEnv) saved(t *testing.T) map[string]any {
	t.Helper()
	raw := env.savedForm.Load()
	if raw == nil {
		t.Fatal("форма не дошла до CRM /save")
	}
	var form map[string]any
	if err := json.Unmarshal([]byte(raw.(string)), &form); err != nil {
		t.Fatalf("форма в CRM не JSON: %v", err)
	}
	return form
}

func postForm(t *testing.T, h http.HandlerFunc, target, jsonBody, referer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", target, strings.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// --- tilda-form ---

func TestTilda_MapsRefererAndLocation(t *testing.T) {
	env := newFormsEnv(t)

	rec := postForm(t, env.forms.SaveTilda, "/tilda-form",
		`{"fullName":"Иван Иванов","phoneNumber":"+7 (777) 123-45-67","locationName":"Алматы"}`,
		"https://ROBOCLEAN.kz/promo?utm=tilda")

	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("легаси-контракт: всегда 200 с пустым телом, получили %d %q", rec.Code, rec.Body.String())
	}

	form := env.saved(t)
	if form["businessAreaId"] != float64(5) { // "roboclean" входит в Referer без учёта регистра
		t.Fatalf("businessAreaId: %v", form["businessAreaId"])
	}
	if form["sourceAppealId"] != float64(7) { // "tilda" входит в Referer (utm=tilda)
		t.Fatalf("sourceAppealId: %v", form["sourceAppealId"])
	}
	if form["locationId"] != float64(12) { // точное имя "Алматы"
		t.Fatalf("locationId: %v", form["locationId"])
	}
	if form["fullName"] != "Иван Иванов" {
		t.Fatalf("fullName: %v", form["fullName"])
	}
	if form["phoneNumber"] != "77771234567" { // только цифры
		t.Fatalf("phoneNumber: %v", form["phoneNumber"])
	}
	if form["note"] != "" {
		t.Fatalf("note: %v", form["note"])
	}
	// решение 2026-07-10: сохранение в CRM идёт БЕЗ токена (permitAll)
	if auth := env.savedAuth.Load().(string); auth != "" {
		t.Fatalf("Authorization не должен уходить в CRM, получили: %q", auth)
	}
	// справочник локаций должен запрашиваться для выбранной businessArea
	if q := env.locQuery.Load().(string); q != "businessAreaId=5" {
		t.Fatalf("locationOptions query: %q", q)
	}
}

func TestTilda_DefaultsWhenNothingMatches(t *testing.T) {
	env := newFormsEnv(t)

	postForm(t, env.forms.SaveTilda, "/tilda-form", `{}`, "https://unknown-site.com/")

	form := env.saved(t)
	if form["businessAreaId"] != float64(defaultBusinessAreaID) {
		t.Fatalf("businessAreaId дефолт: %v", form["businessAreaId"])
	}
	if form["locationId"] != float64(defaultLocationID) {
		t.Fatalf("locationId дефолт: %v", form["locationId"])
	}
	if form["sourceAppealId"] != float64(defaultSourceAppealID) {
		t.Fatalf("sourceAppealId дефолт: %v", form["sourceAppealId"])
	}
	if form["fullName"] != defaultFullName {
		t.Fatalf("fullName дефолт: %v", form["fullName"])
	}
	if form["phoneNumber"] != defaultPhoneNumber {
		t.Fatalf("phoneNumber дефолт: %v", form["phoneNumber"])
	}
}

func TestTilda_UnknownLocationNameFallsBackToDefault(t *testing.T) {
	env := newFormsEnv(t)

	postForm(t, env.forms.SaveTilda, "/tilda-form",
		`{"locationName":"Марс"}`, "")

	if form := env.saved(t); form["locationId"] != float64(defaultLocationID) {
		t.Fatalf("неизвестный город должен давать дефолт %d: %v", defaultLocationID, form["locationId"])
	}
}

func TestTilda_ReferencesDown_LeadStillSavedWithDefaults(t *testing.T) {
	env := newFormsEnv(t)
	env.refsDown.Store(true)

	rec := postForm(t, env.forms.SaveTilda, "/tilda-form",
		`{"fullName":"Клиент","phoneNumber":"87001112233"}`, "https://roboclean.kz/")

	// осознанное отличие от PHP (там был бы 500): лид не теряем
	if rec.Code != http.StatusOK {
		t.Fatalf("при недоступных справочниках лид всё равно сохраняется: %d", rec.Code)
	}
	form := env.saved(t)
	if form["businessAreaId"] != float64(defaultBusinessAreaID) {
		t.Fatalf("должен быть дефолт businessArea: %v", form["businessAreaId"])
	}
	if form["fullName"] != "Клиент" || form["phoneNumber"] != "87001112233" {
		t.Fatalf("данные клиента должны сохраниться: %v", form)
	}
}

func TestTilda_CrmSaveFails_StillReturns200(t *testing.T) {
	env := newFormsEnv(t)
	env.saveStatus.Store(500)

	rec := postForm(t, env.forms.SaveTilda, "/tilda-form", `{"fullName":"X"}`, "")

	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("легаси: Tilda всегда получает 200 без тела, получили %d %q", rec.Code, rec.Body.String())
	}
}

// --- web-form ---

func TestWebForm_NormalKeys(t *testing.T) {
	env := newFormsEnv(t)

	rec := postForm(t, env.forms.SaveWeb, "/web-form",
		`{"businessAreaId":5,"fullName":"Пётр","locationId":12,"phoneNumber":"87071112233","sourceAppealId":7}`, "")

	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("ответ: %d %q", rec.Code, rec.Body.String())
	}
	form := env.saved(t)
	if form["businessAreaId"] != float64(5) || form["locationId"] != float64(12) ||
		form["phoneNumber"] != "87071112233" || form["sourceAppealId"] != float64(7) {
		t.Fatalf("нормальные ключи должны работать (фикс PHP-бага): %v", form)
	}
}

func TestWebForm_LegacyDollarKeysStillWorkAndWin(t *testing.T) {
	env := newFormsEnv(t)

	postForm(t, env.forms.SaveWeb, "/web-form",
		`{"businessAreaId":5,"fullName":"Пётр","$locationId":15,"locationId":12,"$phoneNumber":"111","phoneNumber":"222","$sourceAppealId":11,"sourceAppealId":7}`, "")

	form := env.saved(t)
	// PHP читал только $-ключи — они приоритетнее для совместимости
	if form["locationId"] != float64(15) || form["phoneNumber"] != "111" || form["sourceAppealId"] != float64(11) {
		t.Fatalf("$-ключи (легаси PHP) должны побеждать: %v", form)
	}
}
