package cursor

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	cursorv1 "github.com/tonycoder-hub/cursor-api-proxy/gen/cursorv1"
)

func fixtureJWT(claims string) string {
	encode := base64.RawURLEncoding.EncodeToString
	return encode([]byte(`{"alg":"none"}`)) + "." + encode([]byte(claims)) + ".fixture-signature"
}

func TestTokenExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	exp := now.Add(time.Hour)
	cases := []struct {
		name, token string
		want        time.Time
	}{
		{"jwt exp", fixtureJWT(fmt.Sprintf(`{"exp":%d}`, exp.Unix())), exp.Add(-tokenExpirySkew)},
		{"jwt fractional exp", fixtureJWT(fmt.Sprintf(`{"exp":%d.5}`, exp.Unix())), exp.Add(-tokenExpirySkew)},
		{"jwt without exp", fixtureJWT(`{"sub":"fixture"}`), now.Add(exchangeTTL)},
		{"jwt invalid exp", fixtureJWT(`{"exp":"soon"}`), now.Add(exchangeTTL)},
		{"jwt negative exp", fixtureJWT(`{"exp":-1}`), now.Add(exchangeTTL)},
		{"bad payload", "a.%%%.c", now.Add(exchangeTTL)},
		{"opaque", "fixture-opaque-token", now.Add(exchangeTTL)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tokenExpiry(tc.token, now); !got.Equal(tc.want) {
				t.Fatalf("expiry=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestTokenCacheFollowsJWTExpiry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		lifetime  time.Duration
		exchanges int32
	}{
		{"long-lived token is reused", time.Hour, 1},
		{"token expiring within skew is not reused", tokenExpirySkew / 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var exchanges atomic.Int32
			token := fixtureJWT(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(tc.lifetime).Unix()))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				exchanges.Add(1)
				fmt.Fprintf(w, `{"accessToken":%q}`, token)
			}))
			defer server.Close()
			client := NewClient("fixture-cursor-key")
			client.RPCBase, client.HTTP = server.URL, server.Client()
			for range 2 {
				if got, err := client.accessToken(context.Background()); err != nil || got != token {
					t.Fatalf("token=%q err=%v", got, err)
				}
			}
			if exchanges.Load() != tc.exchanges {
				t.Fatalf("exchanges=%d want=%d", exchanges.Load(), tc.exchanges)
			}
		})
	}
}

// authFixture is a synthetic Cursor upstream that hands out numbered tokens and
// rejects the tokens listed in its policies.
type authFixture struct {
	exchanges, appends, streams atomic.Int32
	appendStatus                func(token string) int
	streamStatus                func(token string) int
	trailer                     string
}

func (f *authFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("Authorization")
	switch r.URL.Path {
	case "/auth/exchange_user_api_key":
		fmt.Fprintf(w, `{"accessToken":"fixture-token-%d"}`, f.exchanges.Add(1))
	case procBidiAppend:
		f.appends.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(f.appendStatus(token))
	case procRunSSE:
		f.streams.Add(1)
		if status := f.streamStatus(token); status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		for _, message := range []*cursorv1.AgentServerMessage{
			{InteractionUpdate: &cursorv1.InteractionUpdate{TextDelta: &cursorv1.TextDeltaUpdate{Text: "hello"}}},
			{InteractionUpdate: &cursorv1.InteractionUpdate{TurnEnded: &cursorv1.TurnEndedUpdate{}}},
		} {
			payload, _ := proto.Marshal(message)
			_, _ = w.Write(testFrame(0, payload))
		}
		trailer := f.trailer
		if trailer == "" {
			trailer = "{}"
		}
		_, _ = w.Write(testFrame(flagEndStream, []byte(trailer)))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func rejectToken(rejected string, status int) func(string) int {
	return func(token string) int {
		if token == "Bearer "+rejected {
			return status
		}
		return http.StatusOK
	}
}

func always(status int) func(string) int { return func(string) int { return status } }

func runFixture(t *testing.T, fixture *authFixture) (*Client, string, error) {
	t.Helper()
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	client := NewClient("fixture-cursor-key")
	client.RPCBase, client.HTTP = server.URL, server.Client()
	var text string
	err := client.Run(context.Background(), "test-model", "hi", IDs{ConversationID: "agent-test", RunID: "test-run"}, func(chunk TextChunk) error {
		text += chunk.Text
		return nil
	})
	return client, text, err
}

func TestRejectedTokenIsRefreshedAndAppendRetriedOnce(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			fixture := &authFixture{appendStatus: rejectToken("fixture-token-1", status), streamStatus: always(http.StatusOK)}
			_, text, err := runFixture(t, fixture)
			if err != nil || text != "hello" {
				t.Fatalf("text=%q err=%v", text, err)
			}
			if fixture.exchanges.Load() != 2 || fixture.appends.Load() != 2 || fixture.streams.Load() != 1 {
				t.Fatalf("exchanges=%d appends=%d streams=%d", fixture.exchanges.Load(), fixture.appends.Load(), fixture.streams.Load())
			}
		})
	}
}

func TestPersistentAuthRejectionIsRetriedOnlyOnce(t *testing.T) {
	fixture := &authFixture{appendStatus: always(http.StatusForbidden), streamStatus: always(http.StatusOK)}
	_, _, err := runFixture(t, fixture)
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.Op != "BidiAppend" || statusErr.Status != http.StatusForbidden {
		t.Fatalf("err=%v", err)
	}
	if fixture.exchanges.Load() != 2 || fixture.appends.Load() != 2 || fixture.streams.Load() != 0 {
		t.Fatalf("exchanges=%d appends=%d streams=%d", fixture.exchanges.Load(), fixture.appends.Load(), fixture.streams.Load())
	}
}

func TestRunSSEOpenIsRetriedAfterAuthRejection(t *testing.T) {
	fixture := &authFixture{appendStatus: always(http.StatusOK), streamStatus: rejectToken("fixture-token-1", http.StatusUnauthorized)}
	_, text, err := runFixture(t, fixture)
	if err != nil || text != "hello" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if fixture.exchanges.Load() != 2 || fixture.streams.Load() != 2 {
		t.Fatalf("exchanges=%d streams=%d", fixture.exchanges.Load(), fixture.streams.Load())
	}
}

func TestNonAuthFailureIsNotRetried(t *testing.T) {
	fixture := &authFixture{appendStatus: always(http.StatusOK), streamStatus: always(http.StatusBadGateway)}
	_, _, err := runFixture(t, fixture)
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.Status != http.StatusBadGateway {
		t.Fatalf("err=%v", err)
	}
	if fixture.exchanges.Load() != 1 || fixture.streams.Load() != 1 {
		t.Fatalf("exchanges=%d streams=%d", fixture.exchanges.Load(), fixture.streams.Load())
	}
}

func TestUnauthenticatedTrailerInvalidatesTokenWithoutRetry(t *testing.T) {
	fixture := &authFixture{appendStatus: always(http.StatusOK), streamStatus: always(http.StatusOK), trailer: `{"error":{"code":"unauthenticated"}}`}
	client, text, err := runFixture(t, fixture)
	var trailerErr *TrailerError
	if !errors.As(err, &trailerErr) || trailerErr.Code != "unauthenticated" || text != "hello" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if fixture.streams.Load() != 1 {
		t.Fatalf("streamed request was retried: streams=%d", fixture.streams.Load())
	}
	client.tok.mu.Lock()
	cached := client.tok.accessToken
	client.tok.mu.Unlock()
	if cached != "" {
		t.Fatal("rejected token is still cached")
	}
}

func TestInvalidateTokenKeepsNewerToken(t *testing.T) {
	client := NewClient("fixture-cursor-key")
	client.tok.accessToken, client.tok.expiresAt = "fixture-token-2", time.Now().Add(time.Minute)
	client.invalidateToken("fixture-token-1")
	if client.tok.accessToken != "fixture-token-2" {
		t.Fatal("stale invalidation discarded a newer token")
	}
	client.invalidateToken("fixture-token-2")
	if client.tok.accessToken != "" {
		t.Fatal("rejected token is still cached")
	}
}
