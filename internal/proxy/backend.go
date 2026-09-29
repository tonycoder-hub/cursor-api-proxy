package proxy

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tonycoder-hub/cursor-api-proxy/internal/config"
	"github.com/tonycoder-hub/cursor-api-proxy/internal/cursor"
)

type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type Backend interface {
	Models(context.Context) ([]Model, error)
	Run(context.Context, string, string, func(cursor.TextChunk) error) error
}

type CursorBackend struct {
	client      *cursor.Client
	modelsURL   string
	mu          sync.Mutex
	catalog     []Model
	expires     time.Time
	refreshDone chan struct{}
}

func NewCursorBackend(cfg config.Config) *CursorBackend {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = true
	transport.MaxIdleConnsPerHost = cfg.MaxConcurrent * 2
	transport.ResponseHeaderTimeout = 30 * time.Second
	client := cursor.NewClient(cfg.CursorAPIKey)
	client.RPCBase = cfg.RPCBaseURL
	client.HTTP = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &CursorBackend{client: client, modelsURL: cfg.ModelsURL}
}

func (b *CursorBackend) Models(ctx context.Context) ([]Model, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b.mu.Lock()
		if len(b.catalog) != 0 && time.Now().Before(b.expires) {
			models := append([]Model(nil), b.catalog...)
			b.mu.Unlock()
			return models, nil
		}
		if done := b.refreshDone; done != nil {
			b.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-done:
				continue
			}
		}
		done := make(chan struct{})
		b.refreshDone = done
		b.mu.Unlock()
		models, err := b.fetchModels(ctx)
		b.mu.Lock()
		if err == nil {
			b.catalog, b.expires = models, time.Now().Add(5*time.Minute)
		}
		b.refreshDone = nil
		close(done)
		b.mu.Unlock()
		return append([]Model(nil), models...), err
	}
}

func (b *CursorBackend) fetchModels(ctx context.Context) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.modelsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create model discovery request failed")
	}
	req.Header.Set("Authorization", "Bearer "+b.client.APIKey)
	req.Header.Set("Accept", "application/json")
	resp, err := b.client.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("model discovery request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model discovery upstream status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, fmt.Errorf("invalid model discovery response")
	}
	var result struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("invalid model discovery JSON")
	}
	seen := map[string]bool{}
	models := []Model{}
	for _, item := range result.Items {
		id := strings.TrimSpace(item.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, Model{ID: id, Object: "model", OwnedBy: "cursor"})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("Cursor returned no usable models")
	}
	return models, nil
}

func (b *CursorBackend) Run(ctx context.Context, model, prompt string, callback func(cursor.TextChunk) error) error {
	return b.client.Run(ctx, model, prompt, cursor.IDs{ConversationID: "agent-" + newUUID(), RunID: newUUID()}, callback)
}

func newUUID() string {
	var id [16]byte
	_, _ = rand.Read(id[:])
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16])
}
