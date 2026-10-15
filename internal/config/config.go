// Package config — вся конфигурация моста берётся из переменных окружения.
// Мост не хранит никаких секретов в коде; переходные креды сервисного
// аккаунта (см. TASK-rewrite-bridge.md в легаси-репо target-bridge, «переходный режим») живут только в env
// и удаляются после парных правок в werp_java_back_v2.
//
// Один деплой = одно окружение (решение 2026-07-11): prod-мост смотрит на
// 192.168.0.15, dev-мост — на 192.168.0.25 (см. .env.prod.example /
// .env.dev.example). Смесь stage/prod апстримов старого PHP-моста не переносится.
package config

import (
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"
)

type Config struct {
	// HTTP-сервер моста
	ListenAddr string

	// Апстримы WERP (внутренняя сеть)
	AuthURL    string // auth-server, /oauth/token
	CoreURL    string // core-модуль, /api/core/...
	ServiceURL string // service-модуль, /api/service/...
	CrmURL     string // werp_crm, /api/crm2/...

	// Внешние апстримы (мост как egress)
	PkbURL    string // ПКБ (First Credit Bureau); тест и прод — разные хосты, дефолта нет
	TwilioURL string // api.twilio.com

	// Приёмник входящих сообщений Twilio (отдельный хост, полный URL).
	// Жив, сохраняется — подтверждено 2026-07-11.
	TwilioReceiveForwardURL string

	// Переходный режим: сервисный аккаунт для партнёрских эндпойнтов,
	// пока core не проверяет партнёрские креденшелы сам.
	// Токен запрашивается на КАЖДЫЙ запрос и нигде не кэшируется.
	OAuthClientID     string
	OAuthClientSecret string
	ServiceUsername   string
	ServicePassword   string

	// Kassa24: статический токен партнёра (проверка уедет в core) и IP-allowlist.
	// Токен обязателен: пустой означал бы «все платёжные запросы отвергаются»,
	// причём молча — поэтому проверяется на старте.
	Kassa24StaticToken string
	Kassa24AllowedIPs  []string // пусто = проверка IP выключена

	// Доверенные reverse-прокси (nginx перед мостом). Заголовки
	// X-Real-Ip/X-Forwarded-For учитываются ТОЛЬКО от этих адресов —
	// иначе клиентский IP берётся из TCP-соединения (защита IP-allowlist
	// Kassa24 и rate limit от подделки заголовков).
	// Пусто = заголовкам не верим никогда.
	TrustedProxies []netip.Prefix

	// Таймауты исходящих запросов к апстримам
	UpstreamTimeout time.Duration

	// Лимит размера входящего тела запроса (байт); 0 = без лимита.
	// Защита независимо от client_max_body_size на nginx.
	MaxBodyBytes int64

	// Rate limiting: запросов в секунду на один клиентский IP
	RateLimitPerSecond int
	RateLimitBurst     int

	KgScheduleRatePerMinute int
	KgScheduleRateBurst     int
}

func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr: getenv("BRIDGE_LISTEN_ADDR", ":8080"),

		AuthURL:    os.Getenv("WERP_AUTH_URL"),
		CoreURL:    os.Getenv("WERP_CORE_URL"),
		ServiceURL: os.Getenv("WERP_SERVICE_URL"),
		CrmURL:     os.Getenv("WERP_CRM_URL"),

		// у ПКБ дефолта осознанно нет: молчаливый уход тестового моста на
		// боевое бюро (или наоборот) хуже, чем отказ на старте
		PkbURL:    os.Getenv("PKB_URL"),
		TwilioURL: getenv("TWILIO_URL", "https://api.twilio.com"),

		TwilioReceiveForwardURL: os.Getenv("TWILIO_RECEIVE_FORWARD_URL"),

		OAuthClientID:     os.Getenv("WERP_OAUTH_CLIENT_ID"),
		OAuthClientSecret: os.Getenv("WERP_OAUTH_CLIENT_SECRET"),
		ServiceUsername:   os.Getenv("WERP_SERVICE_USERNAME"),
		ServicePassword:   os.Getenv("WERP_SERVICE_PASSWORD"),

		Kassa24StaticToken: os.Getenv("KASSA24_STATIC_TOKEN"),

		UpstreamTimeout:    getduration("BRIDGE_UPSTREAM_TIMEOUT", 30*time.Second),
		MaxBodyBytes:       int64(getint("BRIDGE_MAX_BODY_BYTES", 10<<20)), // 10 МБ
		RateLimitPerSecond: getint("BRIDGE_RATE_LIMIT_RPS", 20),
		RateLimitBurst:     getint("BRIDGE_RATE_LIMIT_BURST", 40),

		KgScheduleRatePerMinute: getint("BRIDGE_KG_SCHEDULE_RATE_PER_MIN", 5),
		KgScheduleRateBurst:     getint("BRIDGE_KG_SCHEDULE_RATE_BURST", 5),
	}

	if ips := os.Getenv("KASSA24_ALLOWED_IPS"); ips != "" {
		for _, ip := range strings.Split(ips, ",") {
			cfg.Kassa24AllowedIPs = append(cfg.Kassa24AllowedIPs, strings.TrimSpace(ip))
		}
	}

	if raw := os.Getenv("BRIDGE_TRUSTED_PROXIES"); raw != "" {
		for _, s := range strings.Split(raw, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			p, err := parsePrefix(s)
			if err != nil {
				return nil, fmt.Errorf("BRIDGE_TRUSTED_PROXIES: %q: %w", s, err)
			}
			cfg.TrustedProxies = append(cfg.TrustedProxies, p)
		}
	}

	var missing []string
	for _, v := range []struct{ name, val string }{
		{"WERP_AUTH_URL", cfg.AuthURL},
		{"WERP_CORE_URL", cfg.CoreURL},
		{"WERP_SERVICE_URL", cfg.ServiceURL},
		{"WERP_CRM_URL", cfg.CrmURL},
		{"PKB_URL", cfg.PkbURL},
		{"TWILIO_RECEIVE_FORWARD_URL", cfg.TwilioReceiveForwardURL},
		{"KASSA24_STATIC_TOKEN", cfg.Kassa24StaticToken},
		{"WERP_OAUTH_CLIENT_ID", cfg.OAuthClientID},
		{"WERP_OAUTH_CLIENT_SECRET", cfg.OAuthClientSecret},
		{"WERP_SERVICE_USERNAME", cfg.ServiceUsername},
		{"WERP_SERVICE_PASSWORD", cfg.ServicePassword},
	} {
		if v.val == "" {
			missing = append(missing, v.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("не заданы обязательные переменные окружения: %s", strings.Join(missing, ", "))
	}

	return cfg, nil
}

// parsePrefix принимает CIDR ("192.168.0.0/16") или одиночный IP ("127.0.0.1").
func parsePrefix(s string) (netip.Prefix, error) {
	if !strings.Contains(s, "/") {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		return netip.PrefixFrom(addr, addr.BitLen()), nil
	}
	return netip.ParsePrefix(s)
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getint(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

func getduration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
