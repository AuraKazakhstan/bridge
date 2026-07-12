// Package twilio — WhatsApp через Twilio: входящие вебхуки → внутренний
// приёмник; исходящие (send/messages) → api.twilio.com. Перенос из PHP
// TwilioController (группа B, см. TASK-rewrite-bridge.md в легаси-репо target-bridge).
//
// Приёмник /api/messages/receive (отдельный хост, env TWILIO_RECEIVE_FORWARD_URL)
// ЖИВ и сохраняется — подтверждено пользователем 2026-07-11.
//
// Исходящие (send/messages): мост НЕ хранит креды Twilio (решение
// 2026-07-11) — вызывающий (werp_crm, BridgeFeignClient) передаёт
// Authorization: Basic base64(accountSid:authToken); мост достаёт accountSid
// из заголовка для URL и пробрасывает авторизацию в api.twilio.com.
//
// Осознанные отличия от PHP:
//   - receive: из-за `'json' => $request` на приёмник уходило ПУСТОЕ тело
//     "{}" — новый мост шлёт реальные данные вебхука (query + form) как JSON;
//     при выкатке сверить с приёмником;
//   - send: ошибка Twilio -> 502 с телом ошибки (в PHP SDK кидал исключение
//     и Laravel отдавал 500) — вызывающему (Feign) любой не-2xx равнозначен;
//   - send/messages: нет заголовка Authorization -> 400 (в PHP креды были
//     захардкожены, случая не существовало).
package twilio

import (
	"bytes"
	stdbase64 "encoding/base64"
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

// Handler обслуживает /twilio/send, /twilio/messages, /twilio/receive.
type Handler struct {
	apiURL            string // https://api.twilio.com
	receiveForwardURL string // полный URL приёмника входящих
	auth              *auth.Client
	client            *http.Client
}

func New(apiURL, receiveForwardURL string, authClient *auth.Client, timeout time.Duration) *Handler {
	return &Handler{
		apiURL:            strings.TrimRight(apiURL, "/"),
		receiveForwardURL: receiveForwardURL,
		auth:              authClient,
		client:            &http.Client{Timeout: timeout},
	}
}

// accountSID достаёт SID из Authorization: Basic base64(sid:token).
// Мост НЕ сохраняет креды — SID нужен только для построения URL Twilio.
func accountSID(r *http.Request) (sid, authHeader string, ok bool) {
	authHeader = r.Header.Get("Authorization")
	const prefix = "Basic "
	if !strings.HasPrefix(authHeader, prefix) {
		return "", "", false
	}
	decoded, err := stdbase64.StdEncoding.DecodeString(authHeader[len(prefix):])
	if err != nil {
		return "", "", false
	}
	sid, _, found := strings.Cut(string(decoded), ":")
	if !found || sid == "" {
		return "", "", false
	}
	return sid, authHeader, true
}

const authRequired = `{"error":"Authorization (Basic accountSid:authToken) is required"}`

// Send — POST /twilio/send: исходящее WhatsApp-сообщение.
// Вход (JSON, форма werp_crm TwilioMessageForm): from, to, contentSid,
// body, contentVariables.
func (h *Handler) Send(w http.ResponseWriter, r *http.Request) {
	sid, authHeader, ok := accountSID(r)
	if !ok {
		httpx.WriteJSON(w, http.StatusBadRequest, []byte(authRequired))
		return
	}

	data := httpx.RequestData(r)
	str := func(key string) string {
		if s, ok2 := data[key].(string); ok2 {
			return s
		}
		return ""
	}

	// маппинг PHP send(): whatsapp:+ префиксы, опциональные contentSid/body,
	// contentVariables json-кодируются
	form := url.Values{}
	form.Set("From", "whatsapp:+"+str("from"))
	form.Set("To", "whatsapp:+"+str("to"))
	if v := str("contentSid"); v != "" {
		form.Set("ContentSid", v)
	}
	if v := str("body"); v != "" {
		form.Set("Body", v)
	}
	if cv, ok2 := data["contentVariables"]; ok2 && cv != nil {
		if encoded, err := json.Marshal(cv); err == nil {
			// PHP empty(): пустые contentVariables ({}/[]/"") не отправляются
			switch s := string(encoded); s {
			case "null", "{}", "[]", `""`:
			default:
				form.Set("ContentVariables", s)
			}
		}
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		h.apiURL+"/2010-04-01/Accounts/"+url.PathEscape(sid)+"/Messages.json",
		strings.NewReader(form.Encode()))
	if err != nil {
		httpx.WriteJSON(w, http.StatusBadGateway, []byte(`{"error":"upstream unavailable"}`))
		return
	}
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := h.client.Do(req)
	if err != nil {
		slog.Error("twilio send: api.twilio.com недоступен", "request_id", r.Header.Get("X-Request-Id"), "err", err)
		httpx.WriteJSON(w, http.StatusBadGateway, []byte(`{"error":"upstream unavailable"}`))
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode >= 300 {
		slog.Error("twilio send: сообщение отвергнуто",
			"request_id", r.Header.Get("X-Request-Id"), "status", resp.StatusCode, "twilio_response", string(body))
		httpx.WriteJSON(w, http.StatusBadGateway, body)
		return
	}

	// ответ PHP responseForm: from/to без префикса whatsapp:+, direction, body, sid
	var msg map[string]any
	_ = json.Unmarshal(body, &msg)
	getStr := func(key string) string {
		if s, ok2 := msg[key].(string); ok2 {
			return strings.ReplaceAll(s, "whatsapp:+", "")
		}
		return ""
	}
	out, _ := json.Marshal(map[string]string{
		"from":      getStr("from"),
		"to":        getStr("to"),
		"direction": getStr("direction"),
		"body":      getStr("body"),
		"sid":       getStr("sid"),
	})
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ListMessages — GET /twilio/messages: история сообщений с фильтрами.
// Легаси-контракт PHP: ЛЮБОЙ исход — 200 с обёрткой {status, message, data|error}.
func (h *Handler) ListMessages(w http.ResponseWriter, r *http.Request) {
	sid, authHeader, ok := accountSID(r)
	if !ok {
		httpx.WriteJSON(w, http.StatusBadRequest, []byte(authRequired))
		return
	}

	in := r.URL.Query()
	q := url.Values{}
	setIf := func(twilioKey, inKey, prefix string) {
		if v := strings.TrimSpace(in.Get(inKey)); v != "" {
			q.Set(twilioKey, prefix+v)
		}
	}
	setIf("DateSent<", "dateTo", "")
	setIf("DateSent>", "dateFrom", "")
	setIf("From", "from", "whatsapp:+")
	setIf("To", "to", "whatsapp:+")
	setIf("PageSize", "pageSize", "")
	setIf("PageToken", "pageToken", "")
	setIf("Page", "page", "")

	// легаси: непустой параметр url заменяет собой всю query (пагинация)
	rawQuery := q.Encode()
	if u := strings.TrimSpace(in.Get("url")); u != "" {
		rawQuery = strings.TrimPrefix(u, "?")
	}

	target := h.apiURL + "/2010-04-01/Accounts/" + url.PathEscape(sid) + "/Messages.json"
	if rawQuery != "" {
		target += "?" + rawQuery
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		writeWrapped(w, "error", "An error occurred while sending the message.", "error", err.Error())
		return
	}
	req.Header.Set("Authorization", authHeader)

	resp, err := h.client.Do(req)
	if err != nil {
		writeWrapped(w, "error", "An error occurred while sending the message.", "error", err.Error())
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	var payload any
	if json.Unmarshal(body, &payload) != nil {
		payload = nil
	}
	if resp.StatusCode >= 300 {
		writeWrapped(w, "error", "Failed to send message.", "error", payload)
		return
	}
	writeWrapped(w, "success", "Message sent successfully!", "data", payload)
}

// writeWrapped — обёртка PHP messages(): всегда HTTP 200.
func writeWrapped(w http.ResponseWriter, status, message, payloadKey string, payload any) {
	out, _ := json.Marshal(map[string]any{
		"status":   status,
		"message":  message,
		payloadKey: payload,
	})
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Receive — POST /twilio/receive: вебхук Twilio о входящем сообщении.
func (h *Handler) Receive(w http.ResponseWriter, r *http.Request) {
	requestID := r.Header.Get("X-Request-Id")

	if h.receiveForwardURL == "" {
		slog.Error("twilio receive: TWILIO_RECEIVE_FORWARD_URL не сконфигурирован — сообщение потеряно", "request_id", requestID)
		httpx.WriteJSON(w, http.StatusBadGateway, []byte(`{"error":"receiver not configured"}`))
		return
	}

	body, err := json.Marshal(httpx.RequestData(r))
	if err != nil {
		httpx.WriteJSON(w, http.StatusInternalServerError, []byte(`{"error":"internal"}`))
		return
	}

	token, err := h.auth.FetchToken(r.Context())
	if err != nil {
		slog.Error("twilio receive: transitional token", "request_id", requestID, "err", err)
		httpx.WriteJSON(w, http.StatusBadGateway, []byte(`{"error":"auth upstream unavailable"}`))
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, h.receiveForwardURL, bytes.NewReader(body))
	if err != nil {
		httpx.WriteJSON(w, http.StatusBadGateway, []byte(`{"error":"upstream unavailable"}`))
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		// 502 -> Twilio сам перепошлёт вебхук
		slog.Error("twilio receive: приёмник недоступен", "request_id", requestID, "webhook", string(body), "err", err)
		httpx.WriteJSON(w, http.StatusBadGateway, []byte(`{"error":"upstream unavailable"}`))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		slog.Error("twilio receive: приёмник отверг сообщение",
			"request_id", requestID, "status", resp.StatusCode, "webhook", string(body), "receiver_response", string(b))
		httpx.WriteJSON(w, http.StatusBadGateway, []byte(`{"error":"receiver rejected message"}`))
		return
	}

	w.WriteHeader(http.StatusOK)
}

// Echo возвращает вызывающему его же данные (query + form) — поведение
// технических Twilio-эндпойнтов старого PHP-моста (`return $request;`):
// twilio/receive1, twilio/fallback, twilio/callback.
func Echo(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm() // объединяет query и form-body

	payload := make(map[string]any, len(r.Form))
	for k, v := range r.Form {
		if len(v) == 1 {
			payload[k] = v[0]
		} else {
			payload[k] = v
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}
