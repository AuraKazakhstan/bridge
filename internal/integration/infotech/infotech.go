// Package infotech — delivery-репорты SMS-провайдера (info-tech,
// Infobip-style формат) → core. Перенос из PHP NotifyController (группа B,
// см. TASK-rewrite-bridge.md в легаси-репо target-bridge).
//
// Контракт:
//   - вход: GET /info-tech-status с snake_case-полями (query или тело);
//   - маппинг в camelCase-форму NotifyForm; отсутствующее поле получает
//     значение-заглушку "default_<имя>" (легаси);
//   - форма уходит в core /api/core/sms-info-tech/create-report под
//     переходным сервисным токеном (провайдер WERP-токена не имеет);
//   - ответ вызывающему ВСЕГДА 200 с JSON самой формы (результат отправки
//     в core игнорировался в PHP) — ошибка уходит только в лог.
package infotech

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yesmukhamedov/bridge/internal/auth"
	"github.com/yesmukhamedov/bridge/internal/httpx"
)

// reportForm — исходящая форма (PHP NotifyForm): порядок и имена полей 1:1
type reportForm struct {
	BulkID    any `json:"bulkId"`
	MessageID any `json:"messageId"`
	ExtraID   any `json:"extraId"`
	To        any `json:"to"`
	Sender    any `json:"sender"`
	SentAt    any `json:"sentAt"`
	DoneAt    any `json:"doneAt"`
	SmsCount  any `json:"smsCount"`
	Status    any `json:"status"`
	Mnc       any `json:"mnc"`
	Err       any `json:"err"`
}

// Handler обслуживает GET /info-tech-status.
type Handler struct {
	coreURL string
	auth    *auth.Client
	client  *http.Client
}

func New(coreURL string, authClient *auth.Client, timeout time.Duration) *Handler {
	return &Handler{
		coreURL: strings.TrimRight(coreURL, "/"),
		auth:    authClient,
		client:  &http.Client{Timeout: timeout},
	}
}

// DeliveryReport — GET /info-tech-status.
func (h *Handler) DeliveryReport(w http.ResponseWriter, r *http.Request) {
	data := httpx.RequestData(r)
	pick := func(key string) any {
		if v, ok := data[key]; ok && v != nil {
			return v
		}
		return "default_" + key // легаси-заглушка PHP
	}

	form := reportForm{
		BulkID:    pick("bulk_id"),
		MessageID: pick("message_id"),
		ExtraID:   pick("extra_id"),
		To:        pick("to"),
		Sender:    pick("sender"),
		SentAt:    pick("sent_at"),
		DoneAt:    pick("done_at"),
		SmsCount:  pick("sms_count"),
		Status:    pick("status"),
		Mnc:       pick("mnc"),
		Err:       pick("err"),
	}

	body, err := json.Marshal(form)
	if err != nil {
		httpx.WriteJSON(w, http.StatusInternalServerError, []byte(`{"error":"internal"}`))
		return
	}

	// как в PHP: результат отправки в core не влияет на ответ провайдеру
	if err := h.send(r, body); err != nil {
		slog.Error("infotech: delivery-репорт не дошёл до core (ответ провайдеру всё равно 200 — легаси)",
			"request_id", r.Header.Get("X-Request-Id"), "report", string(body), "err", err)
	}

	httpx.WriteJSON(w, http.StatusOK, body) // легаси: эхо формы
}

func (h *Handler) send(r *http.Request, body []byte) error {
	token, err := h.auth.FetchToken(r.Context())
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		h.coreURL+"/api/core/sms-info-tech/create-report", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &httpx.UpstreamError{Status: resp.StatusCode, Body: b}
	}
	return nil
}
