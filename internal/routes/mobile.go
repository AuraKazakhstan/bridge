package routes

import (
	"net/http"

	"github.com/yesmukhamedov/bridge/internal/auth"
	"github.com/yesmukhamedov/bridge/internal/config"
	"github.com/yesmukhamedov/bridge/internal/proxy"
)

// mountMobileRoutes — мобильное приложение: явный allowlist 1:1 из PHP
// MobileController. Сквозной префикс-прокси /api/service/ и /api/core/
// НЕ используется: он открывал бы в интернет всю поверхность обоих
// модулей (решение 2026-07-11). Авторизацию передаёт вызывающий.
func mountMobileRoutes(mux *http.ServeMux, cfg *config.Config, authClient *auth.Client) {
	t := cfg.UpstreamTimeout

	passSame(mux, cfg.ServiceURL, t,
		"GET /api/service/smappl/appList",
		"POST /api/service/smappl/changeOperator",
		"POST /api/service/smappl/editApp",
		"GET /api/service/smappl/getMasterList",
		"GET /api/service/smappl/getOperatorList",

		"GET /api/service/smecam/{id}",
		"PUT /api/service/smecam/edit",
		"GET /api/service/smecam-ma/{id}",
		"PUT /api/service/smecam-ma-status-update",

		"GET /api/service/smcs/getServiceApplication",
		"GET /api/service/smcs/getServicePackageList",
		"GET /api/service/smcs/getMatnrPriceList",
		"GET /api/service/smcs/getServiceList",
		"POST /api/service/smcs/getPositionSum",
		"POST /api/service/smcs/check",
		"POST /api/service/smcs/master/draft/create-and-payment",
		"GET /api/service/smcs/master/draft/my-list",
		"GET /api/service/smcs/master/draft/{id}",
		"GET /api/service/smcs/master/my-services",

		"GET /api/service/smcuspor/serviceCrmHistoryService",
		"GET /api/service/smvs/{id}",
		"GET /api/service/report/srlsm",
	)
	passSame(mux, cfg.CoreURL, t,
		"GET /api/core/finance/mainoperation/fetchCashBankHkontsByBranch",

		"GET /api/core/logistics/accountabilities",
		"POST /api/core/logistics/accountabilities",
		"GET /api/core/logistics/accountabilities/staff-matnrs/{staffId}",
		"GET /api/core/logistics/accountabilities/{id}",
		"DELETE /api/core/logistics/accountabilities/{id}",
		"GET /api/core/logistics/matnrs-in-werks/{werks}/active-matnrs",
	)

	// ПЕРЕХОДНЫЙ режим: публичные вызывающие (промо-сайт, сервисный сайт,
	// проверка изделия по заводскому номеру) не имеют WERP-токена — мост берёт
	// сервисный токен на КАЖДЫЙ запрос, без кэша (см. internal/auth).
	// Удаляется после правок в core.
	//
	// Штрихкоды статусов изделий: в PHP-мосте searchRobocleanProductByBarcode /
	// searchCebilonProductByBarcode слали апстриму 'Bearer '.get_token(), а НЕ
	// авторизацию вызывающего (MobileController.php:226-245) — при слепом
	// проксировании core отвечал 401 Unauthorized.
	passServiceToken(mux, authClient, cfg.CoreURL, t, map[string]string{
		"GET /api/roboclean-status/by-barcode/{barcode}": "/api/core/logistics/matnr-reports/roboclean/search-by-barcode/{barcode}",
		"GET /api/cebilon-status/by-barcode/{barcode}":   "/api/core/logistics/matnr-reports/cebilon/search-by-barcode/{barcode}",
	})

	mux.Handle("POST /api/promo-20year-uz/ticket-check",
		withServiceToken(authClient, proxy.New(cfg.CoreURL, rewriteTo("/api/core/promo-20year-uz/ticket-check"), t)))
	mux.Handle("GET /api/service-site/master-list",
		withServiceToken(authClient, proxy.New(cfg.CoreURL, rewriteTo("/api/core/hr/staff/master-list"), t)))
}
