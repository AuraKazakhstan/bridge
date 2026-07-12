package routes

import (
	"net/http"

	"github.com/yesmukhamedov/bridge/internal/config"
	"github.com/yesmukhamedov/bridge/internal/integration/target"
)

// mountTargetRoutes — интеграция target (лиды в CRM-модуль target):
// открытые справочники + приём форм Tilda и веб-сайтов.
// Без авторизации: /api/crm2/target/** — permitAll (решение 2026-07-10).
// Исторически весь мост назывался target-bridge, потому что начинался
// с этих эндпойнтов; теперь это просто одна из интеграций.
func mountTargetRoutes(mux *http.ServeMux, cfg *config.Config) {
	t := cfg.UpstreamTimeout

	// справочники
	pass(mux, cfg.CrmURL, t, map[string]string{
		"GET /business-areas":                            "/api/crm2/target/businessAreaOptions",
		"GET /business-areas/{businessAreaId}/locations": "/api/crm2/target/locationOptions",
		"GET /source-appeals":                            "/api/crm2/target/sourceAppealOptions",
	})

	// формы заявок (трансформер, см. internal/integration/target)
	h := target.New(cfg.CrmURL, t)
	mux.HandleFunc("POST /tilda-form", h.SaveTilda)
	mux.HandleFunc("POST /web-form", h.SaveWeb)
}
