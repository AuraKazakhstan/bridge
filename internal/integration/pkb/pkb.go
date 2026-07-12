// Package pkb — ПКБ (Первое кредитное бюро / First Credit Bureau):
// кредитный рейтинг по ИИН. Перенос из PHP FCBController (группа B,
// см. TASK-rewrite-bridge.md в легаси-репо target-bridge).
//
// Принцип соблюдён изначально: креды ПКБ передаёт ВЫЗЫВАЮЩИЙ в заголовке
// `base64` (Basic-авторизация), мост ничего не хранит.
//
// Контракт:
//   - нет заголовка base64 -> 400 {"error":"\"base64\" is required"};
//   - двухшаговый вызов: GET /availability/{iin} (результат отбрасывается,
//     важен только успех), затем GET /scor/{iin} с Consent-Confirmed: 1;
//   - успех -> 200 с телом scor как есть;
//   - ошибка ЛЮБОГО из шагов -> 200 (!) с телом {"iin": <иин>, ...поля
//     ошибки ПКБ} (array_merge из PHP: поля ошибки поверх iin);
//   - тело ошибки не-JSON -> 200 {"iin": <иин>}.
//
// Осознанные отличия от PHP:
//   - verify=false НЕ перенесён — TLS-проверка включена;
//   - сетевая ошибка (ПКБ недоступен) -> 502 (в PHP был fatal 500).
package pkb

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yesmukhamedov/bridge/internal/httpx"
)

// Handler обслуживает GET /credit-rating/iin/{iin}.
type Handler struct {
	baseURL string
	client  *http.Client
}

func New(baseURL string, timeout time.Duration) *Handler {
	return &Handler{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: timeout},
	}
}

// CreditRatingByIIN — GET /credit-rating/iin/{iin}.
func (h *Handler) CreditRatingByIIN(w http.ResponseWriter, r *http.Request) {
	iin := r.PathValue("iin")

	base64 := r.Header.Get("base64")
	if base64 == "" {
		httpx.WriteJSON(w, http.StatusBadRequest, []byte(`{"error":"\"base64\" is required"}`))
		return
	}

	// шаг 1: availability — результат не нужен, важен только успех
	_, err := h.get(r, "/availability/"+iin, base64, false)
	if err != nil {
		h.writeError(w, r, iin, err)
		return
	}

	// шаг 2: собственно рейтинг
	score, err := h.get(r, "/scor/"+iin, base64, true)
	if err != nil {
		h.writeError(w, r, iin, err)
		return
	}

	httpx.WriteJSON(w, http.StatusOK, score)
}

// get выполняет запрос к ПКБ. Не-2xx превращается в *httpx.UpstreamError
// с телом ответа (как Guzzle RequestException).
func (h *Handler) get(r *http.Request, path, base64 string, withConsent bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, h.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Basic "+base64)
	if withConsent {
		req.Header.Set("Consent-Confirmed", "1")
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, &httpx.UpstreamError{Status: resp.StatusCode, Body: body}
	}
	return body, nil
}

// writeError — легаси-контракт PHP: ошибка ПКБ уходит вызывающему со
// статусом 200 и телом {"iin": ..., ...поля ошибки} (поля ошибки поверх iin).
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, iin string, err error) {
	var ue *httpx.UpstreamError
	if !errors.As(err, &ue) {
		// сеть/таймаут — ПКБ недоступен (в PHP это был fatal)
		slog.Error("pkb: ПКБ недоступен", "request_id", r.Header.Get("X-Request-Id"), "err", err)
		httpx.WriteJSON(w, http.StatusBadGateway, []byte(`{"error":"upstream unavailable"}`))
		return
	}

	merged := map[string]any{"iin": iin}
	var fields map[string]any
	if json.Unmarshal(ue.Body, &fields) == nil {
		for k, v := range fields {
			merged[k] = v // как array_merge: поля ошибки побеждают iin
		}
	}
	body, _ := json.Marshal(merged)
	httpx.WriteJSON(w, http.StatusOK, body)
}
