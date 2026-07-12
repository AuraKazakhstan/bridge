package routes

import (
	"net/http"
	"strings"

	"github.com/yesmukhamedov/bridge/internal/auth"
	"github.com/yesmukhamedov/bridge/internal/config"
	"github.com/yesmukhamedov/bridge/internal/integration/infotech"
	"github.com/yesmukhamedov/bridge/internal/integration/kassa24"
	"github.com/yesmukhamedov/bridge/internal/integration/pkb"
	"github.com/yesmukhamedov/bridge/internal/integration/twilio"
	"github.com/yesmukhamedov/bridge/internal/middleware"
)

// mountPartnerRoutes — партнёрские интеграции (трансформеры из
// internal/integration). Возвращает пути, освобождённые от rate limit:
// потерянный из-за 429 платёж или вебхук дороже гипотетического флуда
// (у Kassa24 к тому же есть IP-allowlist).
func mountPartnerRoutes(mux *http.ServeMux, cfg *config.Config, authClient *auth.Client, ipres *middleware.IPResolver) map[string]bool {
	t := cfg.UpstreamTimeout

	// Kassa24 — платёжный провайдер: трансформер (перенос PHP 1:1) +
	// IP-allowlist партнёра. Выборочный перенос query-параметров —
	// поведение PHP: остальное отбрасывается.
	k24 := kassa24.New(cfg.CoreURL, cfg.Kassa24StaticToken, authClient, t)
	ipGuard := middleware.IPAllowlist(cfg.Kassa24AllowedIPs, ipres)
	kassa24Routes := map[string]http.Handler{
		"POST /payments":         http.HandlerFunc(k24.CreatePayment),
		"GET /accept-payments":   k24.ProxyGet("/api/core/finance/kassa24/accept-payments"),
		"GET /company":           k24.ProxyGet("/api/core/finance/kassa24/company"),
		"GET /contract":          k24.ProxyGet("/api/core/finance/kassa24/contract", "bukrsId", "customerId"),
		"GET /customer":          k24.ProxyGet("/api/core/finance/kassa24/customer", "bukrsId", "iin"),
		"GET /get-payments":      k24.ProxyGet("/api/core/finance/kassa24/get-payments", "contractNumber"),
		"GET /payments":          k24.ProxyGet("/api/core/finance/kassa24/payments", "contractNumber"),
		"GET /contract-payments": k24.ProxyGet("/api/core/finance/kassa24/contract-payments", "bukrsId", "customerId"),
	}
	rateExempt := make(map[string]bool, len(kassa24Routes)+2)
	for pattern, h := range kassa24Routes {
		mux.Handle(pattern, ipGuard(h))
		rateExempt[pattern[strings.IndexByte(pattern, ' ')+1:]] = true
	}
	rateExempt["/info-tech-status"] = true
	rateExempt["/twilio/receive"] = true

	// ПКБ: креды передаёт вызывающий (заголовок base64), TLS-проверка включена
	p := pkb.New(cfg.PkbURL, t)
	mux.HandleFunc("GET /credit-rating/iin/{iin}", p.CreditRatingByIIN)

	// Delivery-репорты SMS-провайдера (переходный токен)
	it := infotech.New(cfg.CoreURL, authClient, t)
	mux.HandleFunc("GET /info-tech-status", it.DeliveryReport)

	// Twilio: входящие вебхуки → приёмник (жив, сохраняется — 2026-07-11);
	// исходящие — креды передаёт вызывающий (werp_crm), мост не хранит
	tw := twilio.New(cfg.TwilioURL, cfg.TwilioReceiveForwardURL, authClient, t)
	mux.HandleFunc("POST /twilio/receive", tw.Receive)
	mux.HandleFunc("POST /twilio/send", tw.Send)
	mux.HandleFunc("GET /twilio/messages", tw.ListMessages)
	// технические echo-эндпойнты (PHP возвращал запрос обратно)
	mux.HandleFunc("GET /twilio/receive1", twilio.Echo)
	mux.HandleFunc("POST /twilio/fallback", twilio.Echo)
	mux.HandleFunc("POST /twilio/callback", twilio.Echo)

	return rateExempt
}
