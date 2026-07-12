// Package auth — ПЕРЕХОДНЫЙ механизм (см. TASK-rewrite-bridge.md в легаси-репо target-bridge).
//
// Для партнёрских эндпойнтов (Kassa24, sms-info-tech, twilio/receive) третьи
// стороны не могут передать WERP-токен, поэтому до парных правок в core мост
// получает сервисный токен сам.
//
// РЕШЕНИЕ ПОЛЬЗОВАТЕЛЯ (2026-07-10): токен запрашивается на КАЖДЫЙ запрос
// заново и НИГДЕ не сохраняется — ни в памяти, ни в кэше. Не добавлять сюда
// кэширование. Весь пакет удаляется, когда core начнёт проверять партнёрские
// креденшелы сам (permitAll + фильтр).
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	tokenURL     string
	clientID     string
	clientSecret string
	username     string
	password     string
	http         *http.Client
}

func New(authBaseURL, clientID, clientSecret, username, password string, timeout time.Duration) *Client {
	return &Client{
		tokenURL:     strings.TrimRight(authBaseURL, "/") + "/oauth/token",
		clientID:     clientID,
		clientSecret: clientSecret,
		username:     username,
		password:     password,
		http:         &http.Client{Timeout: timeout},
	}
}

// FetchToken запрашивает свежий access_token сервисного аккаунта.
// Вызывается на каждый запрос; результат используется один раз и отбрасывается.
func (c *Client) FetchToken(ctx context.Context) (string, error) {
	status, raw, err := c.PasswordGrant(ctx, c.username, c.password)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("auth-server ответил %d", status)
	}

	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", fmt.Errorf("некорректный ответ auth-server: %w", err)
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("auth-server вернул пустой access_token")
	}
	return body.AccessToken, nil
}

// PasswordGrant выполняет password-grant с произвольными кредами пользователя
// (Basic OAuth-клиента добавляет мост). Используется FetchToken'ом и легаси
// POST /token, где username/password присылает вызывающий.
// Возвращает статус и тело ответа auth-server без интерпретации.
func (c *Client) PasswordGrant(ctx context.Context, username, password string) (int, []byte, error) {
	form := url.Values{
		"grant_type": {"password"},
		"username":   {username},
		"password":   {password},
	}

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
