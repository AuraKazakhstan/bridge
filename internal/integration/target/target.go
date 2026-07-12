// Package target — приём заявок (лидов) в CRM-модуль target: формы Tilda
// и веб-сайтов. Перенос из PHP FormController легаси-репо target-bridge
// (группа B, задачник TASK-rewrite-bridge.md в легаси-репо).
//
// Исторически весь мост назывался target-bridge, потому что начинался с этих
// эндпойнтов; теперь target/tilda — просто одна из интеграций.
//
// Контракт:
//   - оба эндпойнта ВСЕГДА отвечают 200 с пустым телом (PHP игнорировал
//     результат save()) — Tilda-вебхуку большего и не нужно; ошибки уходят
//     в лог моста;
//   - сервисный токен НЕ используется: CRM /api/crm2/target/** — permitAll
//     (зафиксированное решение 2026-07-10), PHP-шный get_token был мёртвым
//     грузом и сюда не перенесён.
//
// Осознанные отличия от PHP (задокументированы в задачнике):
//   - недоступность справочников CRM не роняет запрос 500-кой: форма
//     сохраняется с дефолтами (лид дороже точного businessArea), ошибка — в лог;
//   - /web-form принимает и нормальные ключи (locationId), и багованные
//     PHP-ключи с литеральным долларом ("$locationId", приоритетнее — так
//     работал PHP для тех, кто слал именно их);
//   - tilda: ПУСТОЙ phoneNumber в form-body у PHP перетирал значение на ""
//     (цикл по $_POST после проверки), здесь — легаси-дефолт 77777777777,
//     как у PHP для JSON-тел: лид с заглушкой заметнее лида с пустым телефоном.
package target

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yesmukhamedov/bridge/internal/httpx"
)

// дефолты PHP saveTildaForm
const (
	defaultBusinessAreaID = 3
	defaultLocationID     = 8
	defaultSourceAppealID = 4
	defaultFullName       = "fullName"
	defaultPhoneNumber    = "77777777777"
)

// targetForm — исходящая форма в CRM (PHP class TargetForm, тот же состав полей)
type targetForm struct {
	BusinessAreaID any    `json:"businessAreaId"`
	FullName       any    `json:"fullName"`
	LocationID     any    `json:"locationId"`
	Note           string `json:"note"`
	PhoneNumber    any    `json:"phoneNumber"`
	SourceAppealID any    `json:"sourceAppealId"`
}

type crmOption struct {
	ID   json.Number `json:"id"`
	Name string      `json:"name"`
}

// Handler обслуживает POST /tilda-form и POST /web-form.
type Handler struct {
	crmURL string
	client *http.Client
}

func New(crmURL string, timeout time.Duration) *Handler {
	return &Handler{
		crmURL: strings.TrimRight(crmURL, "/"),
		client: &http.Client{Timeout: timeout},
	}
}

// SaveTilda — POST /tilda-form: маппинг сырой формы Tilda в TargetForm.
func (h *Handler) SaveTilda(w http.ResponseWriter, r *http.Request) {
	data := httpx.RequestData(r)
	requestID := r.Header.Get("X-Request-Id")
	referer := strings.ToLower(r.Header.Get("Referer"))

	// businessArea: подстрока имени справочника в Referer (без учёта регистра)
	businessAreaID := any(defaultBusinessAreaID)
	if options, err := h.fetchOptions(r.Context(), "/api/crm2/target/businessAreaOptions", nil); err != nil {
		slog.Error("tilda-form: справочник businessArea недоступен, использую дефолт",
			"request_id", requestID, "err", err)
	} else {
		if opt, ok := matchByReferer(options, referer); ok {
			businessAreaID = opt.ID
		}
	}

	// fullName: непустое значение из формы, иначе легаси-дефолт
	fullName := any(defaultFullName)
	if v, ok := nonEmptyString(data, "fullName"); ok {
		fullName = v
	}

	// location: точное имя из формы -> id по справочнику выбранной businessArea
	locationID := any(defaultLocationID)
	if locationName, ok := nonEmptyString(data, "locationName"); ok {
		q := url.Values{"businessAreaId": {toString(businessAreaID)}}
		if options, err := h.fetchOptions(r.Context(), "/api/crm2/target/locationOptions", q); err != nil {
			slog.Error("tilda-form: справочник location недоступен, использую дефолт",
				"request_id", requestID, "err", err)
		} else {
			for _, opt := range options {
				if opt.Name == locationName {
					locationID = opt.ID
					break
				}
			}
		}
	}

	// phone: только цифры, иначе легаси-заглушка
	phone := any(defaultPhoneNumber)
	if v, ok := nonEmptyString(data, "phoneNumber"); ok {
		phone = digitsOnly(v)
	}

	// sourceAppeal: тоже по Referer
	sourceAppealID := any(defaultSourceAppealID)
	if options, err := h.fetchOptions(r.Context(), "/api/crm2/target/sourceAppealOptions", nil); err != nil {
		slog.Error("tilda-form: справочник sourceAppeal недоступен, использую дефолт",
			"request_id", requestID, "err", err)
	} else {
		if opt, ok := matchByReferer(options, referer); ok {
			sourceAppealID = opt.ID
		}
	}

	h.save(r.Context(), requestID, targetForm{
		BusinessAreaID: businessAreaID,
		FullName:       fullName,
		LocationID:     locationID,
		Note:           "",
		PhoneNumber:    phone,
		SourceAppealID: sourceAppealID,
	})

	w.WriteHeader(http.StatusOK) // легаси: всегда 200 без тела
}

// SaveWeb — POST /web-form: значения приходят уже готовыми.
func (h *Handler) SaveWeb(w http.ResponseWriter, r *http.Request) {
	data := httpx.RequestData(r)

	// PHP читал ключи "$locationId"/"$phoneNumber"/"$sourceAppealId" с
	// литеральным долларом (баг) — принимаем оба варианта, доллар приоритетнее
	pick := func(key string) any {
		if v, ok := data["$"+key]; ok {
			return v
		}
		return data[key] // отсутствие -> null, CRM разберётся
	}

	h.save(r.Context(), r.Header.Get("X-Request-Id"), targetForm{
		BusinessAreaID: data["businessAreaId"],
		FullName:       data["fullName"],
		LocationID:     pick("locationId"),
		Note:           "",
		PhoneNumber:    pick("phoneNumber"),
		SourceAppealID: pick("sourceAppealId"),
	})

	w.WriteHeader(http.StatusOK) // легаси: всегда 200 без тела
}

// save отправляет форму в CRM. Ошибка не влияет на ответ вызывающему
// (легаси-контракт), но обязана попасть в лог — иначе лид теряется молча.
func (h *Handler) save(ctx context.Context, requestID string, form targetForm) {
	body, err := json.Marshal(form)
	if err != nil {
		slog.Error("form save: marshal", "request_id", requestID, "err", err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		h.crmURL+"/api/crm2/target/save", bytes.NewReader(body))
	if err != nil {
		slog.Error("form save: request", "request_id", requestID, "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// Authorization не ставится: /api/crm2/target/** — permitAll

	resp, err := h.client.Do(req)
	if err != nil {
		slog.Error("form save: CRM недоступна — ЛИД ПОТЕРЯН", "request_id", requestID, "form", string(body), "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		slog.Error("form save: CRM отвергла форму — ЛИД ПОТЕРЯН",
			"request_id", requestID, "status", resp.StatusCode, "form", string(body), "crm_response", string(b))
	}
}

func (h *Handler) fetchOptions(ctx context.Context, path string, q url.Values) ([]crmOption, error) {
	u := h.crmURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var options []crmOption
	if err := json.NewDecoder(resp.Body).Decode(&options); err != nil {
		return nil, err
	}
	return options, nil
}

// matchByReferer — поведение PHP stripos(Referer, name): имя справочника
// входит в Referer как подстрока, без учёта регистра. Пустые имена
// пропускаются (в PHP пустая подстрока совпадала со всем — это баг).
func matchByReferer(options []crmOption, refererLower string) (crmOption, bool) {
	for _, opt := range options {
		if opt.Name == "" {
			continue
		}
		if strings.Contains(refererLower, strings.ToLower(opt.Name)) {
			return opt, true
		}
	}
	return crmOption{}, false
}

func nonEmptyString(data map[string]any, key string) (string, bool) {
	v, ok := data[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case int:
		return strconv.Itoa(x)
	default:
		b, _ := json.Marshal(v)
		return strings.Trim(string(b), `"`)
	}
}
