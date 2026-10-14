package cursor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

// exchangeResponse is the body of POST /auth/exchange_user_api_key.
type exchangeResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

// tokenCache holds the exchanged access token and when it stops being reused.
type tokenCache struct {
	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
	refreshDone chan struct{}
}

const (
	// exchangeTTL is how long a cached access token is reused when the token
	// does not carry its own expiry.
	exchangeTTL = 10 * time.Minute
	// tokenExpirySkew re-exchanges a token this long before its JWT exp.
	tokenExpirySkew = time.Minute
)

// accessToken returns a valid RPC bearer token, exchanging the API key if needed.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		c.tok.mu.Lock()
		if c.tok.accessToken != "" && time.Now().Before(c.tok.expiresAt) {
			token := c.tok.accessToken
			c.tok.mu.Unlock()
			return token, nil
		}
		if done := c.tok.refreshDone; done != nil {
			c.tok.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-done:
				continue
			}
		}
		done := make(chan struct{})
		c.tok.refreshDone = done
		c.tok.mu.Unlock()
		token, err := c.exchangeToken(ctx)
		c.tok.mu.Lock()
		if err == nil {
			c.tok.accessToken, c.tok.expiresAt = token, tokenExpiry(token, time.Now())
		}
		c.tok.refreshDone = nil
		close(done)
		c.tok.mu.Unlock()
		return token, err
	}
}

// invalidateToken drops token from the cache after Cursor rejected it. A token
// that a concurrent caller already replaced is left alone.
func (c *Client) invalidateToken(token string) {
	c.tok.mu.Lock()
	defer c.tok.mu.Unlock()
	if c.tok.accessToken == token {
		c.tok.accessToken, c.tok.expiresAt = "", time.Time{}
	}
}

// tokenExpiry returns when a freshly exchanged token should stop being reused:
// shortly before its JWT exp claim when present, otherwise after exchangeTTL.
// The signature is not verified; exp is only a cache hint and a rejected token
// is invalidated on 401/403 anyway.
func tokenExpiry(token string, now time.Time) time.Time {
	if exp, ok := jwtExpiry(token); ok {
		return exp.Add(-tokenExpirySkew)
	}
	return now.Add(exchangeTTL)
}

func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp *float64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == nil {
		return time.Time{}, false
	}
	exp := *claims.Exp
	if math.IsNaN(exp) || exp <= 0 || exp >= 1e11 {
		return time.Time{}, false
	}
	return time.Unix(int64(exp), 0), true
}

func (c *Client) exchangeToken(ctx context.Context) (string, error) {
	url := c.base() + "/auth/exchange_user_api_key"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("exchange_user_api_key: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return "", &StatusError{Op: "exchange_user_api_key", Status: resp.StatusCode}
	}
	var er exchangeResponse
	if err := json.Unmarshal(body, &er); err != nil {
		return "", fmt.Errorf("decode exchange response: %w", err)
	}
	if er.AccessToken == "" {
		return "", fmt.Errorf("exchange returned empty accessToken")
	}
	return er.AccessToken, nil
}
