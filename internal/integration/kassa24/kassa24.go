// Package kassa24 — сопряжение с платёжным провайдером Kassa24.
// Перенос 1:1 из PHP Kassa24Controller (TASK-rewrite-bridge.md в легаси-репо target-bridge, группа B).
//
// Контракт с партнёром (НЕ менять без согласования с Kassa24):
//   - партнёр передаёт свой статический токен в query `access_token`;
//   - при неверном токене ответ ВСЕГДА 200 с легаси-телом (не 401!);
//   - при ошибке апстрима ответ 404 с телом ошибки апстрима (не её статусом);
//   - POST /payments отвечает 200 «Form successfully handled» независимо от
//     результата на апстриме (PHP игнорировал ответ send()) — ошибка уходит
//     только в лог моста.
//
// Целевое состояние (после парной правки в core): проверка access_token
// переезжает в core, мост форвардит слепо; переходный токен-флоу удаляется.
package kassa24

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yesmukhamedov/bridge/internal/auth"
	"github.com/yesmukhamedov/bridge/internal/httpx"
)

// точные легаси-тела PHP-моста при неверном access_token
const (
	invalidTokenObject = `{"error":"invalid_token","error_description":"Cannot convert access token to JSON"}`
	// в PHP это была single-quoted строка: \n и \" — ЛИТЕРАЛЬНЫЕ символы
	invalidTokenString = `Resulted in a 401 \nUnauthorized response:\n{\"error\":\"invalid_token\",\"error_description\":\"Cannot convert access token to JSON\"}\n"`
)

// поля исходящей формы платежа — порядок и состав как в PHP PaymentForm
var paymentFormFields = []string{
	"contractNumber", "bankAccount", "paid", "date", "paymentId", "rrn", "terminal",
}

// Handler обслуживает все роуты Kassa24: приём платежа + GET-справки.
type Handler struct {
	coreURL     string
	staticToken string
	auth        *auth.Client
	client      *http.Client
}

func New(coreURL, staticToken string, authClient *auth.Client, timeout time.Duration) *Handler {
	return &Handler{
		coreURL:     strings.TrimRight(coreURL, "/"),
		staticToken: staticToken,
		auth:        authClient,
		client:      &http.Client{Timeout: timeout},
	}
}

func (h *Handler) tokenValid(r *http.Request) bool {
	got := r.URL.Query().Get("access_token")
	return h.staticToken != "" &&
		subtle.ConstantTimeCompare([]byte(got), []byte(h.staticToken)) == 1
}

// CreatePayment — POST /payments.
func (h *Handler) CreatePayment(w http.ResponseWriter, r *http.Request) {
	if !h.tokenValid(r) {
		httpx.WriteJSON(w, http.StatusOK, []byte(invalidTokenObject))
		return
	}

	data := httpx.RequestData(r)
	form := make(map[string]any, len(paymentFormFields))
	for _, f := range paymentFormFields {
		if v, ok := data[f]; ok {
			form[f] = v
		} else {
			form[f] = nil // PHP: отсутствующее поле уходило null-ом
		}
	}

	// как в PHP: результат отправки на апстрим НЕ влияет на ответ партнёру,
	// но обязан попадать в лог — иначе потерянный платёж не расследовать
	if err := h.postPayment(r, form); err != nil {
		slog.Error("kassa24 createPayment: upstream failure (ответ партнёру всё равно 200 — легаси-контракт)",
			"request_id", r.Header.Get("X-Request-Id"), "err", err)
	}

	httpx.WriteJSON(w, http.StatusOK, []byte(`{"message":"Form successfully handled"}`))
}

func (h *Handler) postPayment(r *http.Request, form map[string]any) error {
	token, err := h.auth.FetchToken(r.Context())
	if err != nil {
		return err
	}

	body, err := json.Marshal(form)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		h.coreURL+"/api/core/finance/kassa24", bytes.NewReader(body))
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
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &httpx.UpstreamError{Status: resp.StatusCode, Body: b}
	}
	return nil
}

// ProxyGet возвращает handler для GET-справок Kassa24: проверка статического
// токена → сервисный Bearer → GET на апстрим с выборочным переносом query.
// queryKeys — единственные query-параметры, которые уходят на апстрим
// (поведение PHP: остальное отбрасывается).
func (h *Handler) ProxyGet(upstreamPath string, queryKeys ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.tokenValid(r) {
			legacy, _ := json.Marshal(invalidTokenString)
			httpx.WriteJSON(w, http.StatusOK, legacy)
			return
		}

		token, err := h.auth.FetchToken(r.Context())
		if err != nil {
			slog.Error("kassa24: transitional token", "path", r.URL.Path, "err", err)
			httpx.WriteJSON(w, http.StatusBadGateway, []byte(`{"error":"auth upstream unavailable"}`))
			return
		}

		q := url.Values{}
		for _, key := range queryKeys {
			if r.URL.Query().Has(key) {
				q.Set(key, r.URL.Query().Get(key))
			}
		}
		u := h.coreURL + upstreamPath
		if enc := q.Encode(); enc != "" {
			u += "?" + enc
		}

		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u, nil)
		if err != nil {
			httpx.WriteJSON(w, http.StatusBadGateway, []byte(`{"error":"upstream unavailable"}`))
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := h.client.Do(req)
		if err != nil {
			slog.Error("kassa24: upstream", "path", r.URL.Path, "err", err)
			httpx.WriteJSON(w, http.StatusBadGateway, []byte(`{"error":"upstream unavailable"}`))
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)

		if resp.StatusCode >= 300 {
			// легаси-контракт PHP: любая ошибка апстрима -> 404 с её телом
			if !json.Valid(body) {
				body = []byte("null") // PHP json_decode(не-JSON) -> null
			}
			httpx.WriteJSON(w, http.StatusNotFound, body)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, body)
	}
}
