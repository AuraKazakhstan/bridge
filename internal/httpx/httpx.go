// Package httpx — общие HTTP-хелперы для обработчиков моста.
// Только то, что нужно минимум двум интеграциям; всё специфичное живёт
// в пакете своей интеграции (internal/integration/...).
package httpx

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
)

// WriteJSON пишет готовое JSON-тело с нужным статусом.
func WriteJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// RequestData повторяет Laravel $request->all(): query + JSON-тело или form,
// тело приоритетнее query. Числа сохраняются как json.Number, чтобы при
// пере-кодировании не менять их представление.
func RequestData(r *http.Request) map[string]any {
	data := make(map[string]any)
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			data[k] = v[0]
		}
	}

	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch {
	case strings.Contains(ct, "json"):
		var body map[string]any
		dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
		dec.UseNumber()
		if err := dec.Decode(&body); err == nil {
			for k, v := range body {
				data[k] = v
			}
		}
	default:
		if err := r.ParseForm(); err == nil {
			for k, v := range r.PostForm {
				if len(v) > 0 {
					data[k] = v[0]
				}
			}
		}
	}
	return data
}

// UpstreamError — не-2xx ответ апстрима: статус + тело для легаси-контрактов,
// где тело ошибки апстрима отдаётся вызывающему (аналог Guzzle RequestException).
type UpstreamError struct {
	Status int
	Body   []byte
}

func (e *UpstreamError) Error() string {
	return "upstream status " + http.StatusText(e.Status) + ": " + string(e.Body)
}
