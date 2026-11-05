// Package auth — ПЕРЕХОДНЫЙ механизм (см. TASK-rewrite-bridge.md в легаси-репо target-bridge).
//
// Для партнёрских эндпойнтов (Kassa24, sms-info-tech, twilio/receive) третьи
// стороны не могут передать WERP-токен, поэтому до парных правок в core мост
// получает сервисный токен сам.
//
// РЕШЕНИЕ ПОЛЬЗОВАТЕЛЯ (2026-11-05, заменяет решение 2026-07-10 «без кэша»):
// токен держится в памяти до истечения, вход выполняется по одному. auth-server
// (AT-1286) хранит одну сессию на пользователя: новый вход отзывает прежние
// токены, а одновременные входы падают с 500. Токен, отвергнутый апстримом (401),
// сбрасывается, запрос повторяется один раз. Весь пакет удаляется, когда core
// начнёт проверять партнёрские креденшелы сам (permitAll + фильтр).
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const tokenExpiryMargin = 30 * time.Second

// ErrToken — сервисный токен получить не удалось (апстрим не вызывался).
var ErrToken = errors.New("сервисный токен не получен")

type Client struct {
	tokenURL     string
	clientID     string
	clientSecret string
	username     string
	password     string
	http         *http.Client

	login     chan struct{}
	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

func New(authBaseURL, clientID, clientSecret, username, password string, timeout time.Duration) *Client {
	return &Client{
		tokenURL:     strings.TrimRight(authBaseURL, "/") + "/oauth/token",
		clientID:     clientID,
		clientSecret: clientSecret,
		username:     username,
		password:     password,
		http:         &http.Client{Timeout: timeout},
		login:        make(chan struct{}, 1),
	}
}

// FetchToken возвращает access_token сервисного аккаунта: из памяти, пока он не
// истёк, иначе выполняет вход. Одновременные вызовы ждут один вход.
func (c *Client) FetchToken(ctx context.Context) (string, error) {
	if token := c.cached(); token != "" {
		return token, nil
	}

	select {
	case c.login <- struct{}{}:
		defer func() { <-c.login }()
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(c.http.Timeout):
		return "", fmt.Errorf("очередь входа в auth-server дольше таймаута")
	}

	if token := c.cached(); token != "" {
		return token, nil
	}

	status, raw, err := c.PasswordGrant(ctx, c.username, c.password)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("auth-server ответил %d", status)
	}

	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", fmt.Errorf("некорректный ответ auth-server: %w", err)
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("auth-server вернул пустой access_token")
	}

	c.mu.Lock()
	c.token = body.AccessToken
	c.expiresAt = time.Now().Add(time.Duration(body.ExpiresIn)*time.Second - tokenExpiryMargin)
	c.mu.Unlock()
	return body.AccessToken, nil
}

func (c *Client) cached() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Now().Before(c.expiresAt) {
		return c.token
	}
	return ""
}

// Invalidate сбрасывает токен, отвергнутый апстримом. Если соседний запрос уже
// получил новый токен, он остаётся: сравнение не даёт входить повторно.
func (c *Client) Invalidate(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == token {
		c.token = ""
		c.expiresAt = time.Time{}
	}
}

// Do выполняет запрос с сервисным токеном; при 401 от апстрима сбрасывает токен
// и повторяет один раз. build вызывается на каждую попытку и создаёт запрос заново.
func (c *Client) Do(ctx context.Context, client *http.Client, build func() (*http.Request, error)) (*http.Response, error) {
	resp, token, err := c.attempt(ctx, client, build)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	resp.Body.Close()
	slog.Warn("сервисный токен отвергнут апстримом, повторный вход", "username", c.username)
	c.Invalidate(token)
	resp, _, err = c.attempt(ctx, client, build)
	return resp, err
}

func (c *Client) attempt(ctx context.Context, client *http.Client, build func() (*http.Request, error)) (*http.Response, string, error) {
	token, err := c.FetchToken(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrToken, err)
	}
	req, err := build()
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	return resp, token, err
}

// PasswordGrant выполняет password-grant с произвольными кредами пользователя
// (Basic OAuth-клиента добавляет мост). Используется FetchToken'ом и легаси
// POST /token, где username/password присылает вызывающий.
// Возвращает статус и тело ответа auth-server без интерпретации.
func (c *Client) PasswordGrant(ctx context.Context, username, password string) (int, []byte, error) {
	return c.tokenRequest(ctx, url.Values{
		"grant_type": {"password"},
		"username":   {username},
		"password":   {password},
	})
}

// RefreshGrant renews a caller's token using the bridge's configured OAuth client.
func (c *Client) RefreshGrant(ctx context.Context, refreshToken string) (int, []byte, error) {
	return c.tokenRequest(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
}

func (c *Client) tokenRequest(ctx context.Context, form url.Values) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.SetBasicAuth(c.clientID, c.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("auth-server недоступен: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("некорректный ответ auth-server: %w", err)
	}
	return resp.StatusCode, body, nil
}
