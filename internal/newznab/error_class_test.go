package newznab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestQueryErrorClassification(t *testing.T) {
	t.Run("retryable_on_429", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		defer srv.Close()

		c := New("test", srv.URL, "key")
		_, err := c.Search("query")
		if err == nil || !IsRetryable(err) {
			t.Fatalf("expected retryable error, got: %v", err)
		}
	})

	t.Run("invalid_on_401", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()

		c := New("test", srv.URL, "key")
		_, err := c.Search("query")
		if err == nil || !IsInvalid(err) {
			t.Fatalf("expected invalid error, got: %v", err)
		}
	})
}

func TestContextDeadlineClassification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"><channel></channel></rss>`))
	}))
	defer srv.Close()

	c := New("test", srv.URL, "key")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	_, err := c.SearchContext(ctx, "query")
	if err == nil || !IsRetryable(err) {
		t.Fatalf("expected retryable timeout error, got: %v", err)
	}
}
