package routes

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
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

// passBody — как pass, но с восстановлением PHP-контракта тела (phpBodyCompat).
// Для роутов, где легаси AituRequestsController@sendRequest слал апстриму
// $request->all() как JSON. Нужен, пока core читает эти параметры из JSON-тела,
// а вызывающие (напр. Aitu) присылают их в query/форме.
func passBody(mux *http.ServeMux, upstream string, timeout time.Duration, table map[string]string) {
	for pattern, upstreamPath := range table {
		mux.Handle(pattern, phpBodyCompat(proxy.New(upstream, rewriteTo(upstreamPath), timeout)))
	}
}

// phpBodyCompat воспроизводит поведение PHP-моста
// (AituRequestsController@sendRequest): тело апстриму уходит как
// application/json, собранное из ОБЪЕДИНЁННЫХ query- и body-параметров —
// Laravel $request->all() сливал query и тело в один массив (тело приоритетнее)
// и Guzzle-опция json сериализовала его. Go-прокси иначе стримит тело как есть,
// из-за чего core не видел, например, OTP-код, присланный вызывающим в query
// или form-urlencoded (см. /credit-rating/check-consent-code).
//
// Контракт сохраняем 1:1, включая мелочи:
//   - query-строка на апстриме остаётся как пришла (PHP слал и query, и json);
//   - пустой результат ($request->all() == []) — тело не отправляем (в PHP
//     if($body) при пустом массиве ложно);
//   - тело неизвестного типа (не json/не form) не трогаем — пробрасываем как есть,
//     чтобы не потерять данные (такие вызовы на этих роутах не встречаются).
func phpBodyCompat(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		merged := map[string]any{}
		// query кладём первыми — тело перекроет при совпадении ключей (как $request->all())
		for k, v := range r.URL.Query() {
			if len(v) > 0 {
				merged[k] = v[0]
			}
		}

		ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		switch {
		case strings.Contains(ct, "json"):
			raw, err := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if err != nil {
				http.Error(w, `{"error":"cannot read body"}`, http.StatusBadRequest)
				return
			}
			if len(bytes.TrimSpace(raw)) > 0 {
				var m map[string]any
				if json.Unmarshal(raw, &m) != nil {
					// не JSON-объект — не рискуем, пробрасываем тело как пришло
					r.Body = io.NopCloser(bytes.NewReader(raw))
					r.ContentLength = int64(len(raw))
					next.ServeHTTP(w, r)
					return
				}
				for k, v := range m {
					merged[k] = v
				}
			}
		case strings.Contains(ct, "x-www-form-urlencoded"):
			if err := r.ParseForm(); err != nil {
				http.Error(w, `{"error":"cannot parse form"}`, http.StatusBadRequest)
				return
			}
			for k, v := range r.PostForm {
				if len(v) > 0 {
					merged[k] = v[0]
				}
			}
		default:
			// Тело неизвестного типа: если оно непустое — не рискуем и
			// пробрасываем как есть. Если тела нет (обычный случай: параметры
			// пришли в query без Content-Type) — соберём JSON из query ниже.
			raw, err := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if err != nil {
				http.Error(w, `{"error":"cannot read body"}`, http.StatusBadRequest)
				return
			}
			if len(bytes.TrimSpace(raw)) > 0 {
				r.Body = io.NopCloser(bytes.NewReader(raw))
				r.ContentLength = int64(len(raw))
				next.ServeHTTP(w, r)
				return
			}
		}

		if len(merged) == 0 {
			// PHP при пустом $request->all() тело не слал
			r.Body = http.NoBody
			r.ContentLength = 0
			r.Header.Del("Content-Type")
			r.Header.Del("Content-Length")
			next.ServeHTTP(w, r)
			return
		}

		buf, err := json.Marshal(merged)
		if err != nil {
			http.Error(w, `{"error":"cannot encode body"}`, http.StatusInternalServerError)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(buf))
		r.ContentLength = int64(len(buf))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Content-Length", strconv.Itoa(len(buf)))
		next.ServeHTTP(w, r)
	})
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
