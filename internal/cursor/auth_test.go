package cursor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTokenRefreshWaiterCanCancel(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		io.WriteString(w, `{"accessToken":"fixture-exchanged-token"}`)
	}))
	defer server.Close()
	defer close(release)
	client := NewClient("fixture-cursor-key")
	client.RPCBase = server.URL
	client.HTTP = server.Client()
	done := make(chan error, 1)
	go func() { _, err := client.accessToken(context.Background()); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("exchange did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	canceled := make(chan error, 1)
	go func() { _, err := client.accessToken(ctx); canceled <- err }()
	select {
	case err := <-canceled:
		if err != context.DeadlineExceeded {
			t.Fatalf("waiter error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter did not observe cancellation")
	}
}
