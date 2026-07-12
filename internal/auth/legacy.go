// Легаси POST /token — перенос PHP AituRequestsController@token 1:1.
// Внешний вызывающий (Aitu-приложение) не умеет передавать Basic OAuth-клиента,
// поэтому его добавляет мост (креды в env, не в коде — в PHP были захардкожены
// 'WERP'/'password'). Прозрачный вариант для новых вызывающих — POST /token/mobile.
//
// Контракт PHP (НЕ менять, пока жив вызывающий):
//   - вход: username/password (query, form или JSON);
//   - успех -> 200 с телом auth-server как есть;
//   - ЛЮБАЯ ошибка (неверные креды, недоступный auth-server) -> 500
//     {"error":"Ошибка при получении токена"} — деталей наружу не отдаём.
package auth

import (
	"log/slog"
	"net/http"

	"github.com/yesmukhamedov/bridge/internal/httpx"
)

// LegacyTokenHandler — обработчик легаси POST /token поверх Client.
type LegacyTokenHandler struct {
	client *Client
}

func NewLegacyTokenHandler(client *Client) *LegacyTokenHandler {
	return &LegacyTokenHandler{client: client}
}

func (h *LegacyTokenHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data := httpx.RequestData(r)
	str := func(key string) string {
		if s, ok := data[key].(string); ok {
			return s
		}
		return ""
	}

	status, body, err := h.client.PasswordGrant(r.Context(), str("username"), str("password"))
	if err != nil || status >= 300 {
		slog.Error("legacy /token: password-grant не прошёл",
			"request_id", r.Header.Get("X-Request-Id"), "status", status, "err", err)
		httpx.WriteJSON(w, http.StatusInternalServerError, []byte(`{"error":"Ошибка при получении токена"}`))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, body)
}
