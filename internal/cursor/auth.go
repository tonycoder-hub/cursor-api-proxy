package cursor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// tokenCache holds the exchanged access token and when it was fetched.
type tokenCache struct {
	mu          sync.Mutex
	accessToken string
	fetchedAt   time.Time
	refreshDone chan struct{}
}

// exchangeTTL is how long a cached access token is reused before re-exchanging.
const exchangeTTL = 10 * time.Minute

// accessToken returns a valid RPC bearer token, exchanging the API key if needed.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		c.tok.mu.Lock()
		if c.tok.accessToken != "" && time.Since(c.tok.fetchedAt) < exchangeTTL {
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
			c.tok.accessToken, c.tok.fetchedAt = token, time.Now()
		}
		c.tok.refreshDone = nil
		close(done)
		c.tok.mu.Unlock()
		return token, err
	}
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
		return "", fmt.Errorf("exchange_user_api_key status %d", resp.StatusCode)
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
