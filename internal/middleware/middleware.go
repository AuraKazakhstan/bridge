// Package middleware — сквозные обработчики моста: request-id, access-лог,
// recover от паник, лимит тела запроса, rate limiting по IP, IP-allowlist
// для партнёрских роутов, CORS для публичных сайтов.
//
// Клиентский IP определяет IPResolver: заголовкам X-Real-Ip/X-Forwarded-For
// мост верит ТОЛЬКО когда TCP-пир — доверенный прокси (nginx из
// BRIDGE_TRUSTED_PROXIES). Иначе любой клиент подделал бы IP Kassa24
// заголовком и прошёл бы allowlist (или обошёл rate limit).
package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"
)

const RequestIDHeader = "X-Request-Id"

// IPResolver вычисляет клиентский IP с учётом доверенных прокси.
type IPResolver struct {
	trusted []netip.Prefix
}

func NewIPResolver(trusted []netip.Prefix) *IPResolver {
	return &IPResolver{trusted: trusted}
}

// ClientIP: если соединение пришло от доверенного прокси — берём X-Real-Ip
// (nginx перетирает его на $remote_addr), иначе последний элемент
// X-Forwarded-For (единственный, добавленный НАШИМ прокси; передние элементы
// контролирует клиент). От недоверенного пира заголовки игнорируются.
func (res *IPResolver) ClientIP(r *http.Request) string {
	peer := remoteIP(r)
	if !res.isTrusted(peer) {
		return peer
	}
	if ip := strings.TrimSpace(r.Header.Get("X-Real-Ip")); ip != "" {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
			return ip
		}
	}
	return peer
}

func (res *IPResolver) isTrusted(ip string) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	for _, p := range res.trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RequestID проставляет уникальный id на каждый запрос (для трассировки
// «дошёл ли платёж» по логам моста и апстримов).
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if id == "" {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		r.Header.Set(RequestIDHeader, id)
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r)
	})
}

// AccessLog — структурированный лог каждого прохода через мост.
func AccessLog(res *IPResolver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		slog.Info("access",
			"request_id", r.Header.Get(RequestIDHeader),
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"ip", res.ClientIP(r),
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap — для http.ResponseController (ReverseProxy флашит стриминговые
// ответы через него).
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// Recover перехватывает панику обработчика: вместо обрыва соединения
// вызывающий получает чистый 500 (вебхуки вроде Twilio на 500 делают
// retry), а паника со стеком попадает в лог с request_id.
// http.ErrAbortHandler пробрасывается дальше — это штатный механизм
// обрыва ответа (им пользуется ReverseProxy при ошибке стриминга).
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			slog.Error("panic",
				"request_id", r.Header.Get(RequestIDHeader),
				"method", r.Method,
				"path", r.URL.Path,
				"panic", rec,
				"stack", string(debug.Stack()),
			)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"internal"}`))
		}()
		next.ServeHTTP(w, r)
	})
}

// MaxBody ограничивает размер входящего тела запроса — независимо от
// настроек nginx перед мостом. Превышение лимита обрывает чтение тела
// у обработчика (http.MaxBytesReader). 0 = без лимита.
func MaxBody(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if limit > 0 && r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RateLimit — простой token-bucket на клиентский IP, без внешних зависимостей.
// Мост торчит в интернет: без лимита любой публичный роут можно долбить.
// exempt возвращает true для запросов вне лимита (партнёрские вебхуки:
// Kassa24 защищена IP-allowlist'ом, а потерянный из-за 429 платёж или
// delivery-репорт дороже гипотетического флуда). nil = лимит для всех.
func RateLimit(rps float64, burst int, res *IPResolver, exempt func(*http.Request) bool) func(http.Handler) http.Handler {
	buckets := &sync.Map{} // ip -> *bucket

	// фоновая чистка старых бакетов
	go func() {
		for range time.Tick(5 * time.Minute) {
			cutoff := time.Now().Add(-10 * time.Minute)
			buckets.Range(func(k, v any) bool {
				if v.(*bucket).last().Before(cutoff) {
					buckets.Delete(k)
				}
				return true
			})
		}
	}()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if exempt != nil && exempt(r) {
				next.ServeHTTP(w, r)
				return
			}
			ip := res.ClientIP(r)
			v, _ := buckets.LoadOrStore(ip, newBucket(rps, burst))
			if !v.(*bucket).allow() {
				http.Error(w, `{"error":"too many requests"}`, http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type bucket struct {
	mu       sync.Mutex
	tokens   float64
	rps      float64
	max      float64
	lastSeen time.Time
}

func newBucket(rps float64, burst int) *bucket {
	return &bucket{tokens: float64(burst), rps: rps, max: float64(burst), lastSeen: time.Now()}
}

func (b *bucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens = min(b.max, b.tokens+now.Sub(b.lastSeen).Seconds()*b.rps)
	b.lastSeen = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (b *bucket) last() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastSeen
}

// IPAllowlist пропускает только перечисленные IP (для Kassa24 — у платёжного
// провайдера фиксированные адреса). Пустой список = проверка выключена.
func IPAllowlist(ips []string, res *IPResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(ips) > 0 && !slices.Contains(ips, res.ClientIP(r)) {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func CORS(origins []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			w.Header().Add("Vary", "Origin")
			if slices.Contains(origins, origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			r.Header.Del("Origin")
			next.ServeHTTP(w, r)
		})
	}
}
