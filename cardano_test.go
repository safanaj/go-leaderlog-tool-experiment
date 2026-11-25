package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestEta0FromHTTP pins the contract of the eta0 endpoint client: success
// trims whitespace, 404 means "not yet available" (empty, no error), and any
// transport/server error is a returned error (never a panic, never a hang).
func TestEta0FromHTTP(t *testing.T) {
	origURL, origTimeout := eta0URL, eta0Timeout
	defer func() { eta0URL, eta0Timeout = origURL, origTimeout }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/nonce.next":
			fmt.Fprint(w, "  deadbeef  \n")
		case "/missing":
			http.NotFound(w, r)
		case "/server-error":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "/slow":
			time.Sleep(2 * time.Second)
			fmt.Fprint(w, "late")
		}
	}))
	defer srv.Close()

	ctx := context.Background()

	t.Run("ok trims surrounding whitespace", func(t *testing.T) {
		eta0URL = srv.URL + "/nonce.next"
		got, err := eta0FromHTTP(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got != "deadbeef" {
			t.Errorf("got %q, want %q", got, "deadbeef")
		}
	})

	t.Run("404 means nonce not yet available", func(t *testing.T) {
		eta0URL = srv.URL + "/missing"
		got, err := eta0FromHTTP(ctx)
		if err != nil {
			t.Fatalf("404 must not be an error: %v", err)
		}
		if got != "" {
			t.Errorf("got %q, want empty string", got)
		}
	})

	t.Run("server error is an error", func(t *testing.T) {
		eta0URL = srv.URL + "/server-error"
		if _, err := eta0FromHTTP(ctx); err == nil {
			t.Error("want error on HTTP 500")
		}
	})

	t.Run("unreachable host is an error, not a panic", func(t *testing.T) {
		eta0URL = "http://127.0.0.1:1/nonce.next" // nothing listens here
		if _, err := eta0FromHTTP(ctx); err == nil {
			t.Error("want error on unreachable host")
		}
	})

	t.Run("timeout is an error", func(t *testing.T) {
		eta0Timeout = 100 * time.Millisecond
		eta0URL = srv.URL + "/slow"
		start := time.Now()
		if _, err := eta0FromHTTP(ctx); err == nil {
			t.Error("want error on timeout")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("timeout fired too late: %v", elapsed)
		}
	})
}
