package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckDBCerts pins the fail-fast validation of the Postgres source's TLS
// material: all three paths required, each a regular file.
func TestCheckDBCerts(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "client.crt")
	key := filepath.Join(dir, "client.key")
	ca := filepath.Join(dir, "ca.crt")
	for _, p := range []string{cert, key, ca} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("all required", func(t *testing.T) {
		err := checkDBCerts(&dbConfig{DSN: defaultDBDSN})
		if err == nil || !strings.Contains(err.Error(), "-db-cert-file") ||
			!strings.Contains(err.Error(), "-db-key-file") ||
			!strings.Contains(err.Error(), "-db-ca-cert-file") {
			t.Errorf("want error naming the three flags, got: %v", err)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		err := checkDBCerts(&dbConfig{Cert: cert, Key: key, CACert: filepath.Join(dir, "nope.crt")})
		if err == nil || !strings.Contains(err.Error(), "-db-ca-cert-file") {
			t.Errorf("want error naming the missing flag, got: %v", err)
		}
	})

	t.Run("not a regular file", func(t *testing.T) {
		err := checkDBCerts(&dbConfig{Cert: dir, Key: key, CACert: ca})
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("want not-a-regular-file error, got: %v", err)
		}
	})

	t.Run("ok", func(t *testing.T) {
		if err := checkDBCerts(&dbConfig{Cert: cert, Key: key, CACert: ca}); err != nil {
			t.Errorf("want nil, got: %v", err)
		}
	})
}

// TestNewDataSourceValidation checks selection errors that need no network.
func TestNewDataSourceValidation(t *testing.T) {
	ctx := context.Background()

	origDS, origSocket := dataSourceStr, nodeSocket
	defer func() { dataSourceStr, nodeSocket = origDS, origSocket }()

	dataSourceStr = "bogus"
	if _, err := newDataSource(ctx); err == nil || !strings.Contains(err.Error(), "unknown -data-source") {
		t.Errorf("want unknown-data-source error, got: %v", err)
	}

	dataSourceStr = "node"
	nodeSocket = ""
	if _, err := newDataSource(ctx); err == nil || !strings.Contains(err.Error(), "-node-socket is required") {
		t.Errorf("want node-socket-required error, got: %v", err)
	}

	nodeSocket = "/does/not/exist.sock"
	if _, err := newDataSource(ctx); err == nil || !strings.Contains(err.Error(), "dial node socket") {
		t.Errorf("want dial error, got: %v", err)
	}

	dataSourceStr = "db"
	if _, err := newDataSource(ctx); err == nil || !strings.Contains(err.Error(), "-db-cert-file") {
		t.Errorf("want cert-validation error, got: %v", err)
	}
}
