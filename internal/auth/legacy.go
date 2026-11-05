// Легаси POST /token — перенос PHP AituRequestsController@token 1:1.
// Внешний вызывающий (Aitu-приложение) не умеет передавать Basic OAuth-клиента,
// поэтому его добавляет мост (креды в env, не в коде — в PHP были захардкожены
// 'WERP'/'password'). Прозрачный вариант для новых вызывающих — POST /token/mobile.
//
// Контракт PHP (НЕ менять, пока жив вызывающий):
//   - вход: username/password (query, form или JSON);
//   - успех -> 200 с телом auth-server как есть;
//   - ЛЮБАЯ ошибка password grant (неверные креды, недоступный auth-server) -> 500
//     {"error":"Ошибка при получении токена"} — деталей наружу не отдаём.
//
// Refresh extension: grant_type=refresh_token forwards only the refresh token;
// OAuth 400/401 errors expose only a standard error code, other failures are generic.
package auth

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

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

	switch str("grant_type") {
	case "", "password":
		// Preserve the original password-grant status and body contract.
	case "refresh_token":
		refresh := str("refresh_token")
		if strings.TrimSpace(refresh) == "" {
			httpx.WriteJSON(w, http.StatusBadRequest, []byte(`{"error":"invalid_request"}`))
			return
		}
		h.refresh(w, r, refresh)
		return
	default:
		httpx.WriteJSON(w, http.StatusBadRequest, []byte(`{"error":"unsupported_grant_type"}`))
		return
	}
	status, body, err := h.client.PasswordGrant(r.Context(), str("username"), str("password"))
	if err != nil || status >= 300 {
		slog.Error("legacy /token: password-grant не прошёл",
			"request_id", r.Header.Get("X-Request-Id"), "status", status)
		httpx.WriteJSON(w, http.StatusInternalServerError, []byte(`{"error":"Ошибка при получении токена"}`))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, body)
}

func (h *LegacyTokenHandler) refresh(w http.ResponseWriter, r *http.Request, token string) {
	status, body, err := h.client.RefreshGrant(r.Context(), token)
	if err != nil {
		httpx.WriteJSON(w, http.StatusServiceUnavailable, []byte(`{"error":"temporarily_unavailable"}`))
		return
	}
	if status == http.StatusOK {
		httpx.WriteJSON(w, http.StatusOK, body)
		return
	}
	if status == http.StatusBadRequest || status == http.StatusUnauthorized {
		var oauth struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(body, &oauth); err != nil {
			oauth.Error = "server_error"
		}
		switch oauth.Error {
		case "invalid_grant", "invalid_token", "unsupported_grant_type", "invalid_client",
			"unauthorized_client", "invalid_request", "invalid_scope", "server_error", "temporarily_unavailable":
		default:
			oauth.Error = "server_error"
		}
		safe, _ := json.Marshal(oauth)
		httpx.WriteJSON(w, status, safe)
		return
	}
	if status == http.StatusServiceUnavailable {
		httpx.WriteJSON(w, http.StatusServiceUnavailable, []byte(`{"error":"temporarily_unavailable"}`))
		return
	}
	httpx.WriteJSON(w, http.StatusBadGateway, []byte(`{"error":"server_error"}`))
}
