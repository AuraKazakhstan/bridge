package routes

import (
	"net/http"

	"github.com/yesmukhamedov/bridge/internal/config"
	"github.com/yesmukhamedov/bridge/internal/proxy"
)

// mountAituRoutes — приложение Aitu (заявки, договоры, e-sign, кредитный
// рейтинг): внешние короткие пути → core /api/core/marketing/... и
// /api/core/reference/... Авторизацию передаёт вызывающий.
func mountAituRoutes(mux *http.ServeMux, cfg *config.Config) {
	t := cfg.UpstreamTimeout

	pass(mux, cfg.CoreURL, t, map[string]string{
		"GET /userList": "/api/core/hr/staff/user-demopad",

		"GET /requests/{id}":                          "/api/core/marketing/aitu/requests/{id}",
		"GET /requests/{id}/biometry":                 "/api/core/marketing/aitu/requests/{id}/biometry",
		"GET /requests/{id}/get-client-data":          "/api/core/marketing/aitu/requests/{id}/get-client-data",
		"GET /requests/{requestId}/customers":         "/api/core/marketing/aitu/requests/{requestId}/customers",
		"GET /requests/{requestId}/contract-details":  "/api/core/marketing/aitu/requests/{requestId}/contract-details",
		"GET /requests/{requestId}/esign/redirect":    "/api/core/marketing/aitu/requests/{requestId}/esign/redirect",
		"GET /contract-types":                         "/api/core/marketing/aitu/contract-types",
		"GET /contract-types/{contractTypeId}/prices": "/api/core/marketing/aitu/contract-types/{contractTypeId}/prices",
		"GET /sub-companies":                          "/api/core/marketing/aitu/sub-companies",
		"GET /pdf/{id}":                               "/api/core/marketing/aitu/docs/download/original/{id}",
		"GET /credit-rating/requestId/{requestId}":    "/api/core/marketing/credit-rating/requestId/{requestId}",
		"GET /userInfo":                               "/api/core/reference/userInfo",
	})

	// Роуты с телом: PHP слал апстриму $request->all() как JSON (query+тело
	// вместе). Восстанавливаем этот контракт — иначе core не видит параметры,
	// присланные вызывающим в query или form (напр. OTP-код check-consent-code).
	passBody(mux, cfg.CoreURL, t, map[string]string{
		"POST /requests":                          "/api/core/marketing/aitu/requests",
		"PATCH /requests/{id}":                    "/api/core/marketing/aitu/requests/{id}",
		"POST /requests/{requestId}/send-to-sign": "/api/core/marketing/aitu/requests/{requestId}/send-to-sign",
		"POST /requests/es/callback":              "/api/core/marketing/aitu/requests/es/callback",
		"PATCH /customers/{id}":                   "/api/core/marketing/aitu/customers/{id}",
		"POST /credit-rating/send-code":           "/api/core/marketing/credit-rating/send-code",
		"POST /credit-rating/check-consent-code":  "/api/core/marketing/credit-rating/check-consent-code",
		"POST /credit-rating/resend-code":         "/api/core/marketing/credit-rating/resend-code",
	})

	// /requests/staff/{staffId} неоднозначен для ServeMux рядом с
	// /requests/{id}/<literal>-роутами (в Laravel решалось порядком
	// регистрации), поэтому диспетчеризуем сегмент "staff" вручную через
	// generic-паттерн; литеральные 3-сегментные роуты выше его специфичнее
	// и выигрывают у него по правилам ServeMux.
	staffProxy := proxy.New(cfg.CoreURL, func(r *http.Request) string {
		return "/api/core/marketing/aitu/requests/staff/" + r.PathValue("p2")
	}, t)
	mux.Handle("GET /requests/{p1}/{p2}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("p1") == "staff" {
			staffProxy.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}))

	// contract-types-list: единственный aitu-роут с трансформацией — мост
	// подмешивает дефолтные query-параметры, если вызывающий их не передал
	// (поведение PHP contractTypesListUrl: array_merge defaults + query).
	mux.Handle("GET /contract-types-list", queryDefaults(map[string]string{
		"direction": "ASC", "page": "0", "size": "50",
	}, proxy.New(cfg.CoreURL, rewriteTo("/api/core/marketing/contract-type"), t)))
}
