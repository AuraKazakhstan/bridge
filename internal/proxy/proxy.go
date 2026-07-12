// Package proxy — ядро моста: слепое перенаправление запроса на апстрим.
// Принципы (задачник TASK-rewrite-bridge.md в легаси-репо target-bridge):
//   - тело запроса и ответа стримится как есть, мост в него не заглядывает;
//   - Authorization и прочие заголовки вызывающего пробрасываются без изменений;
//   - статус-код и тело ошибки апстрима возвращаются честно, без обёрток.
package proxy

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

var (
	transportsMu sync.Mutex
	transports   = map[time.Duration]*http.Transport{}
)

// transportFor — один транспорт (и пул соединений) на все роуты с одинаковым
// таймаутом, вместо отдельного пула на каждый из ~50 роутов к трём хостам.
func transportFor(timeout time.Duration) *http.Transport {
	transportsMu.Lock()
	defer transportsMu.Unlock()
	tr, ok := transports[timeout]
	if !ok {
		tr = &http.Transport{
			ResponseHeaderTimeout: timeout,
			// TLS-проверка НЕ отключается ни для одного апстрима
			// (в старом мосте для ПКБ стояло verify=false — сюда не переносим).
		}
		transports[timeout] = tr
	}
	return tr
}

// Rewrite строит путь на апстриме из входящего запроса.
// nil означает «тот же путь и query, что пришли».
type Rewrite func(r *http.Request) string

// New возвращает handler, который перенаправляет запрос на upstreamBase.
// upstreamBase — базовый URL апстрима (схема + хост[:порт]), без пути.
func New(upstreamBase string, rewrite Rewrite, timeout time.Duration) http.Handler {
	target, err := url.Parse(upstreamBase)
	if err != nil {
		panic("proxy: некорректный upstream URL " + upstreamBase + ": " + err.Error())
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.Host = target.Host

			if rewrite != nil {
				pr.Out.URL.Path = rewrite(pr.In)
			}
			// query остаётся как пришёл (pr.Out наследует его из pr.In)

			// Сообщаем апстриму реальный источник
			pr.SetXForwarded()
		},
		Transport: transportFor(timeout),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Error("upstream error",
				"path", r.URL.Path,
				"upstream", target.Host,
				"err", err,
			)
			http.Error(w, `{"error":"upstream unavailable"}`, http.StatusBadGateway)
		},
	}

	return rp
}
