package middleware

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func resolver(t *testing.T, prefixes ...string) *IPResolver {
	t.Helper()
	var ps []netip.Prefix
	for _, s := range prefixes {
		ps = append(ps, netip.MustParsePrefix(s))
	}
	return NewIPResolver(ps)
}

func req(remoteAddr string, headers map[string]string) *http.Request {
	r := httptest.NewRequest("GET", "/x", nil)
	r.RemoteAddr = remoteAddr
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// Главный сценарий атаки: клиент из интернета подделывает X-Real-Ip под IP
// Kassa24 — заголовок должен игнорироваться, IP берётся из TCP-соединения.
func TestClientIP_SpoofedHeaderFromUntrustedPeerIgnored(t *testing.T) {
	res := resolver(t, "127.0.0.1/32")
	r := req("203.0.113.7:4444", map[string]string{
		"X-Real-Ip":       "185.98.4.1", // «IP Kassa24»
		"X-Forwarded-For": "185.98.4.1",
	})
	if got := res.ClientIP(r); got != "203.0.113.7" {
		t.Fatalf("подделанный заголовок не должен подменять IP: %s", got)
	}
}

func TestClientIP_TrustedProxyHeadersHonored(t *testing.T) {
	res := resolver(t, "127.0.0.1/32", "192.168.0.0/16")

	// nginx ставит X-Real-IP = $remote_addr
	r := req("192.168.0.5:80", map[string]string{"X-Real-Ip": "185.98.4.1"})
	if got := res.ClientIP(r); got != "185.98.4.1" {
		t.Fatalf("X-Real-Ip от доверенного прокси: %s", got)
	}

	// без X-Real-Ip: берётся ПОСЛЕДНИЙ элемент XFF (добавлен нашим прокси);
	// передние элементы контролирует клиент — им не верим
	r = req("192.168.0.5:80", map[string]string{
		"X-Forwarded-For": "6.6.6.6, 185.98.4.1",
	})
	if got := res.ClientIP(r); got != "185.98.4.1" {
		t.Fatalf("последний хоп XFF: %s", got)
	}
}

func TestClientIP_NoTrustedProxies_AlwaysRemoteAddr(t *testing.T) {
	res := resolver(t) // пустой список = заголовкам не верим никогда
	r := req("192.168.0.5:80", map[string]string{"X-Real-Ip": "185.98.4.1"})
	if got := res.ClientIP(r); got != "192.168.0.5" {
		t.Fatalf("без доверенных прокси — только RemoteAddr: %s", got)
	}
}

func TestIPAllowlist_BlocksSpoofAllowsPartner(t *testing.T) {
	res := resolver(t, "127.0.0.1/32")
	guard := IPAllowlist([]string{"185.98.4.1"}, res)
	ok := guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// злоумышленник со спуфом — 403
	rec := httptest.NewRecorder()
	ok.ServeHTTP(rec, req("203.0.113.7:4444", map[string]string{"X-Real-Ip": "185.98.4.1"}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("спуф должен блокироваться: %d", rec.Code)
	}

	// настоящий партнёр через доверенный nginx — 200
	rec = httptest.NewRecorder()
	ok.ServeHTTP(rec, req("127.0.0.1:1234", map[string]string{"X-Real-Ip": "185.98.4.1"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("партнёр должен проходить: %d", rec.Code)
	}
}

func TestRateLimit_ExemptPathBypassesLimit(t *testing.T) {
	res := resolver(t)
	limited := RateLimit(1, 1, res, func(r *http.Request) bool {
		return r.URL.Path == "/payments"
	})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// burst=1: второй обычный запрос с того же IP — 429
	for i, want := range []int{200, 429} {
		rec := httptest.NewRecorder()
		limited.ServeHTTP(rec, req("203.0.113.7:1234", nil))
		if rec.Code != want {
			t.Fatalf("запрос %d: %d, ожидался %d", i, rec.Code, want)
		}
	}

	// платёжный путь вне лимита — сколько угодно подряд
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		r := req("203.0.113.7:1234", nil)
		r.URL.Path = "/payments"
		limited.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("exempt-путь не должен лимитироваться: %d", rec.Code)
		}
	}
}

func TestRecover_PanicBecomes500(t *testing.T) {
	h := Recover(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req("203.0.113.7:1234", nil))

	if rec.Code != http.StatusInternalServerError || rec.Body.String() != `{"error":"internal"}` {
		t.Fatalf("паника должна превращаться в чистый 500: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRecover_ErrAbortHandlerPassesThrough(t *testing.T) {
	h := Recover(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		if recover() != http.ErrAbortHandler {
			t.Fatal("ErrAbortHandler — штатный обрыв ответа (ReverseProxy), его надо пробрасывать")
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), req("203.0.113.7:1234", nil))
	t.Fatal("должна была подняться паника ErrAbortHandler")
}

func TestMaxBody_OversizedBodyCutOff(t *testing.T) {
	read := func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			var mbe *http.MaxBytesError
			if !errors.As(err, &mbe) {
				t.Fatalf("ожидалась MaxBytesError, получили: %v", err)
			}
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}

	h := MaxBody(16)(http.HandlerFunc(read))

	// в лимите — проходит
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/x", strings.NewReader("short"))
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("тело в лимите должно проходить: %d", rec.Code)
	}

	// сверх лимита — чтение обрывается
	rec = httptest.NewRecorder()
	r = httptest.NewRequest("POST", "/x", strings.NewReader(strings.Repeat("A", 64)))
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("тело сверх лимита должно обрываться: %d", rec.Code)
	}
}
