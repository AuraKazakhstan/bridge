// Package routes — декларативная таблица маршрутов моста.
// Это ЕДИНСТВЕННОЕ место, где описано, какие внутренние эндпойнты открыты
// в глобальную сеть (принцип: явный allowlist, всё остальное — 404).
// Обработчики интеграций живут в internal/integration/..., здесь — только
// привязка «внешний путь → обработчик/апстрим».
//
// Файлы пакета — по доменам вызывающих:
//   - token.go    — OAuth-токены (легаси /token, /token/mobile)
//   - target.go   — интеграция target: справочники + формы Tilda/веб-сайтов → CRM
//   - mobile.go   — мобильное приложение → service/core (+переходные public-сайты)
//   - aitu.go     — приложение Aitu (заявки, e-sign) → core
//   - partners.go — партнёрские интеграции: Kassa24, ПКБ, Twilio, SMS-репорты
//
// Источник соответствий: легаси PHP-мост (репо target-bridge),
// routes/web.php + контроллеры.
package routes

import (
	"net/http"

	"github.com/yesmukhamedov/bridge/internal/auth"
	"github.com/yesmukhamedov/bridge/internal/config"
	"github.com/yesmukhamedov/bridge/internal/middleware"
)

// New собирает mux моста.
func New(cfg *config.Config) http.Handler {
	mux := http.NewServeMux()
	ipres := middleware.NewIPResolver(cfg.TrustedProxies)

	// Переходный сервисный аккаунт (партнёрские эндпойнты + легаси /token);
	// токен на КАЖДЫЙ запрос, без кэша (см. internal/auth).
	authClient := auth.New(cfg.AuthURL, cfg.OAuthClientID, cfg.OAuthClientSecret,
		cfg.ServiceUsername, cfg.ServicePassword, cfg.UpstreamTimeout)

	// health-check самого моста
	mux.HandleFunc("GET /ping", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("pong"))
	})

	mountTokenRoutes(mux, cfg, authClient)
	mountTargetRoutes(mux, cfg)
	mountMobileRoutes(mux, cfg, authClient, ipres)
	mountAituRoutes(mux, cfg)
	rateExempt := mountPartnerRoutes(mux, cfg, authClient, ipres)

	// Сквозные middleware (снаружи внутрь):
	// request-id → access-лог → recover → лимит тела → rate limit.
	// Recover внутри AccessLog — чтобы паника попадала в access-лог как 500.
	var h http.Handler = mux
	h = middleware.RateLimit(float64(cfg.RateLimitPerSecond), cfg.RateLimitBurst, ipres,
		func(r *http.Request) bool { return rateExempt[r.URL.Path] })(h)
	h = middleware.MaxBody(cfg.MaxBodyBytes)(h)
	h = middleware.Recover(h)
	h = middleware.AccessLog(ipres, h)
	h = middleware.RequestID(h)
	return h
}
