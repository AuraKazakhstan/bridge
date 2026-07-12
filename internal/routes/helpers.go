package routes

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/yesmukhamedov/bridge/internal/auth"
	"github.com/yesmukhamedov/bridge/internal/proxy"
)

// pass регистрирует набор маршрутов «внешний паттерн → путь на апстриме».
// Плейсхолдеры вида {id} подставляются из внешнего пути.
func pass(mux *http.ServeMux, upstream string, timeout time.Duration, table map[string]string) {
	for pattern, upstreamPath := range table {
		mux.Handle(pattern, proxy.New(upstream, rewriteTo(upstreamPath), timeout))
	}
}

// passSame регистрирует маршруты, у которых путь на апстриме совпадает
// с внешним 1:1 (мобильный allowlist): один прокси на все паттерны.
func passSame(mux *http.ServeMux, upstream string, timeout time.Duration, patterns ...string) {
	p := proxy.New(upstream, nil, timeout)
	for _, pattern := range patterns {
		mux.Handle(pattern, p)
	}
}

// rewriteTo строит Rewrite-функцию: подставляет {плейсхолдеры} из запроса.
func rewriteTo(upstreamPath string) proxy.Rewrite {
	return func(r *http.Request) string {
		path := upstreamPath
		for {
			start := strings.IndexByte(path, '{')
			if start < 0 {
				return path
			}
			end := strings.IndexByte(path[start:], '}')
			if end < 0 {
				return path
			}
			name := path[start+1 : start+end]
			path = path[:start] + r.PathValue(name) + path[start+end+1:]
		}
	}
}

// queryDefaults подставляет query-параметры по умолчанию, если вызывающий
// их не передал (значения вызывающего всегда приоритетнее).
func queryDefaults(defaults map[string]string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		for k, v := range defaults {
			if !q.Has(k) {
				q.Set(k, v)
			}
		}
		r.URL.RawQuery = q.Encode()
		next.ServeHTTP(w, r)
	})
}

// withServiceToken — переходный обработчик: на каждый запрос берёт свежий
// сервисный токен (без кэша) и подставляет его вместо авторизации вызывающего.
func withServiceToken(a *auth.Client, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := a.FetchToken(r.Context())
		if err != nil {
			slog.Error("transitional token", "path", r.URL.Path, "err", err)
			http.Error(w, `{"error":"auth upstream unavailable"}`, http.StatusBadGateway)
			return
		}
		r.Header.Set("Authorization", "Bearer "+token)
		next.ServeHTTP(w, r)
	})
}
