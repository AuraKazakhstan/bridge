package routes

import (
	"net/http"

	"github.com/yesmukhamedov/bridge/internal/auth"
	"github.com/yesmukhamedov/bridge/internal/config"
)

// mountTokenRoutes — OAuth-токены.
// /token/mobile — прозрачный: Basic OAuth-клиента передаёт вызывающий.
// /token — легаси для Aitu-приложения: Basic добавляет мост (креды в env);
// контракт PHP сохранён (см. auth.LegacyTokenHandler).
func mountTokenRoutes(mux *http.ServeMux, cfg *config.Config, authClient *auth.Client) {
	pass(mux, cfg.AuthURL, cfg.UpstreamTimeout, map[string]string{
		"POST /token/mobile": "/oauth/token",
	})
	mux.Handle("POST /token", auth.NewLegacyTokenHandler(authClient))
}
