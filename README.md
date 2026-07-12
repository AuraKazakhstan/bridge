# bridge

Мост между глобальной сетью и внутренними сервисами WERP (Aura Kazakhstan):
внутри — core/service/crm/auth, снаружи — интеграции Kassa24, ПКБ, Twilio,
Aitu, Tilda, мобильное приложение.

Переписан с нуля на Go (stdlib, ноль зависимостей) с легаси-репо
`AuraKazakhstan/target-bridge` (Laravel 8) с сохранением поведения всех
эндпойнтов 1:1. Историческое имя «target-bridge» осталось от первой
интеграции (target/Tilda-формы) — теперь это просто одна из интеграций,
поэтому проект называется просто **bridge**. Постановка, инвентаризация
и решения — в задачнике `TASK-rewrite-bridge.md` легаси-репо (`_notes/`).

## Принципы
1. Мост ничего не хранит (ни БД, ни кэшей, ни токенов).
2. По умолчанию — слепое перенаправление: тело стримится как есть, `Authorization` передаёт вызывающий.
3. Явный allowlist маршрутов (`internal/routes/`) — всё незадекларированное отвечает 404. Это касается и мобильных путей `/api/service/...`, `/api/core/...`: открыт ровно список из PHP MobileController, а не префиксы целиком.
4. **Один деплой = одно окружение** (решение 2026-07-11): prod-мост смотрит только на 192.168.0.15, dev-мост — только на 192.168.0.25. Смесь stage/prod апстримов старого PHP не переносится.
5. Заголовкам `X-Real-Ip`/`X-Forwarded-For` мост верит только от доверенных прокси (`BRIDGE_TRUSTED_PROXIES`) — иначе IP-allowlist Kassa24 и rate limit обходились бы подделкой заголовка.

## Запуск
```sh
cp .env.dev.example .env    # dev/stage-контур (192.168.0.25)
# или .env.prod.example — прод (192.168.0.15)
# заполнить секреты: все пустые переменные обязательны, мост не стартует без них
go run ./cmd/bridge
```

Тесты (дублирующие: фиксируют легаси-контракты PHP 1:1):
```sh
go test ./...
```

## Структура

Разделение — по осям «кто вызывает» (routes) и «с кем интегрируемся» (integration):

```
cmd/bridge/            — main: сервер, graceful shutdown
internal/config/       — конфигурация из env (обязательные переменные проверяются на старте)
internal/httpx/        — общие HTTP-хелперы: RequestData (аналог Laravel $request->all()),
                         WriteJSON, UpstreamError
internal/proxy/        — ядро: ReverseProxy со стримингом, честные ошибки апстрима,
                         общий Transport на все роуты
internal/middleware/   — request-id, access-лог, recover от паник, лимит тела
                         запроса, rate limit по IP, IP-allowlist, IPResolver
internal/auth/         — клиент auth-server: переходный сервисный токен (на каждый
                         запрос, без кэша) + легаси POST /token (LegacyTokenHandler)
internal/integration/  — пакет на внешнюю интеграцию, у каждого свой Handler + тесты:
    kassa24/           — платежи Kassa24 (легаси-контракт партнёра 1:1)
    pkb/               — ПКБ / First Credit Bureau: кредитный рейтинг по ИИН
    twilio/            — WhatsApp: Send/ListMessages (egress), Receive (вебхук), Echo
    target/            — интеграция target: формы заявок Tilda и веб-сайтов → CRM
    infotech/          — delivery-репорты SMS-провайдера → core
internal/routes/       — ЕДИНСТВЕННОЕ место привязки внешних путей (allowlist),
                         по файлу на вызывающего:
    routes.go          — сборка: middleware-цепочка + mount-функции
    token.go           — OAuth-токены (/token, /token/mobile)
    target.go          — интеграция target: справочники + формы Tilda/веб-сайтов → CRM
    mobile.go          — мобильное приложение → service/core (+переходные public-сайты)
    aitu.go            — приложение Aitu (заявки, e-sign) → core
    partners.go        — партнёрские интеграции (kassa24, pkb, twilio, infotech)
```

Внутренние апстримы (core/service/crm/auth) задаются только конфигом — пакетам
интеграций передаются готовые URL, никакой адресации в коде. wazzup через мост
не ходит (werp_call_center интегрируется с ним напрямую); появится — станет
`internal/integration/wazzup/` + строчки в partners.go.

## Миграция трафика
Старый PHP-мост (target-bridge) и bridge живут бок о бок; nginx переключает
пути на новый мост по мере переноса и проверки. Группа A (чистые прокси)
переключается сразу, группа B (трансформеры) — после дублирующих тестов.
