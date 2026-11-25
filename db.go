package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// dbDataSource reads the leader-check inputs directly from a cexplorer
// (cardano-db-sync) Postgres database over a certificate-authenticated TLS
// connection, bypassing Koios entirely.
//
// The queries are the ones verified against the live DB (see PLAN.md §1/§11):
//
//	(a) epoch nonce:        SELECT encode(nonce,'hex') FROM epoch_param WHERE epoch_no = $1
//	(b) epoch block hash:   SELECT encode(b.hash,'hex') FROM epoch_param ep JOIN block b ON b.id=ep.block_id WHERE ep.epoch_no = $1
//	(c) total active stake: SELECT COALESCE(SUM(amount),0) FROM epoch_stake WHERE epoch_no = $1
//	(d) pool active stake:  SELECT COALESCE(SUM(es.amount),0) FROM epoch_stake es JOIN pool_hash ph ON ph.id=es.pool_id
//	                        WHERE ph.view = $1 AND es.epoch_no = $2
//
// The Go/Set/Mark snapshots map to those queries at E-1/E/E+1 (E = current
// epoch). Note epoch_stake "leads" epoch_param by one epoch: the E+1 stake rows
// (the Mark snapshot) appear before the E+1 nonce does, so GetCandidateNonce
// returns "" until the upcoming epoch actually starts — unlike the koios
// (f2lb) and node (chain-dep-state) sources, which compute the candidate nonce
// early.
type dbDataSource struct {
	pool *pgxpool.Pool
	db   *sql.DB
}

var _ DataSource = (*dbDataSource)(nil)

// dbConfig is the configuration for the Postgres data source.
type dbConfig struct {
	DSN    string // keyword-value DSN (host=... port=... dbname=... user=... sslmode=...)
	Cert   string // client TLS certificate (sslcert)
	Key    string // client TLS key (sslkey)
	CACert string // CA certificate (sslrootcert)
}

// dbQueryTimeout bounds each SQL query.
const dbQueryTimeout = 15 * time.Second

// NewDBDataSource connects to the cexplorer database and pings it, so a bad
// DSN or missing/unreadable TLS material fails fast with a clear error.
func NewDBDataSource(ctx context.Context, cfg *dbConfig) (DataSource, error) {
	if err := checkDBCerts(cfg); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("%s sslcert=%s sslkey=%s sslrootcert=%s",
		strings.TrimSpace(cfg.DSN), cfg.Cert, cfg.Key, cfg.CACert)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("creating the cexplorer connection pool: %w", err)
	}
	// NOTE: closing the sql.DB does NOT close the pool; we close both.
	db := stdlib.OpenDBFromPool(pool)

	pingCtx, cancel := context.WithTimeout(ctx, dbQueryTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		pool.Close()
		return nil, fmt.Errorf("connecting to the cexplorer database: %w", err)
	}
	return &dbDataSource{pool: pool, db: db}, nil
}

// checkDBCerts validates the TLS material: all three paths are required and
// must be regular, readable files (mirrors the f2lb checkCerts flow).
func checkDBCerts(cfg *dbConfig) error {
	if cfg.Cert == "" || cfg.Key == "" || cfg.CACert == "" {
		return fmt.Errorf("-db-cert-file, -db-key-file and -db-ca-cert-file are all required for -data-source db")
	}
	for _, p := range []struct{ flag, path string }{
		{"-db-cert-file", cfg.Cert},
		{"-db-key-file", cfg.Key},
		{"-db-ca-cert-file", cfg.CACert},
	} {
		fi, err := os.Stat(p.path)
		if err != nil {
			return fmt.Errorf("%s: %w", p.flag, err)
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("%s: %s is not a regular file", p.flag, p.path)
		}
	}
	return nil
}

func (d *dbDataSource) Name() string { return "db" }

func (d *dbDataSource) Close() error {
	err := d.db.Close()
	d.pool.Close() // pgxpool's Close returns no value
	return err
}

// queryContext applies the per-query timeout.
func (d *dbDataSource) queryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, dbQueryTimeout)
}

// nonceForEpoch is query (a). found is false when the epoch_param row (or its
// nonce) is not present yet — e.g. for the upcoming epoch.
func (d *dbDataSource) nonceForEpoch(ctx context.Context, epoch int) (nonce string, found bool, err error) {
	qctx, cancel := d.queryContext(ctx)
	defer cancel()
	err = d.db.QueryRowContext(qctx,
		`SELECT encode(nonce,'hex') FROM epoch_param WHERE epoch_no = $1`, epoch,
	).Scan(&nonce)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("querying epoch nonce (epoch %d): %w", epoch, err)
	}
	nonce = strings.TrimSpace(nonce)
	if nonce == "" {
		return "", false, nil // row exists but the nonce column is still unset
	}
	return nonce, true, nil
}

// blockHashForEpoch is query (b), informational only: "" when absent.
func (d *dbDataSource) blockHashForEpoch(ctx context.Context, epoch int) (string, error) {
	qctx, cancel := d.queryContext(ctx)
	defer cancel()
	var hash string
	err := d.db.QueryRowContext(qctx,
		`SELECT encode(b.hash,'hex')
		 FROM epoch_param ep JOIN block b ON b.id = ep.block_id
		 WHERE ep.epoch_no = $1`, epoch,
	).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("querying epoch block hash (epoch %d): %w", epoch, err)
	}
	return hash, nil
}

// totalActiveStakeForEpoch is query (c): SUM(epoch_stake.amount) for the epoch,
// 0 when there are no rows (main.go rejects a zero total stake before any
// leader check).
func (d *dbDataSource) totalActiveStakeForEpoch(ctx context.Context, epoch int) (uint64, error) {
	qctx, cancel := d.queryContext(ctx)
	defer cancel()
	// SUM(bigint) returns numeric; cast back to the lovelace domain.
	var total int64
	err := d.db.QueryRowContext(qctx,
		`SELECT COALESCE(SUM(amount),0)::bigint FROM epoch_stake WHERE epoch_no = $1`, epoch,
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("querying total active stake (epoch %d): %w", epoch, err)
	}
	if total < 0 {
		return 0, fmt.Errorf("negative total active stake %d for epoch %d", total, epoch)
	}
	return uint64(total), nil
}

// poolStakeForEpochRow is query (d): the pool's SUM(epoch_stake.amount) for
// the epoch (0 for an unknown pool / no delegation in that epoch).
func (d *dbDataSource) poolStakeForEpochRow(ctx context.Context, epoch int, poolIDBech32 string) (uint64, error) {
	qctx, cancel := d.queryContext(ctx)
	defer cancel()
	var stake int64
	err := d.db.QueryRowContext(qctx,
		`SELECT COALESCE(SUM(es.amount),0)::bigint
		 FROM epoch_stake es JOIN pool_hash ph ON ph.id = es.pool_id
		 WHERE ph.view = $1 AND es.epoch_no = $2`, poolIDBech32, epoch,
	).Scan(&stake)
	if err != nil {
		return 0, fmt.Errorf("querying pool stake (pool %s, epoch %d): %w", poolIDBech32, epoch, err)
	}
	if stake < 0 {
		return 0, fmt.Errorf("negative pool stake %d for pool %s in epoch %d", stake, poolIDBech32, epoch)
	}
	return uint64(stake), nil
}

func (d *dbDataSource) GetEpochInfo(ctx context.Context, epoch int) (EpochInfo, error) {
	nonce, found, err := d.nonceForEpoch(ctx, epoch)
	if err != nil {
		return EpochInfo{}, err
	}
	if !found {
		return EpochInfo{}, fmt.Errorf("no nonce for epoch %d in the cexplorer database", epoch)
	}
	total, err := d.totalActiveStakeForEpoch(ctx, epoch)
	if err != nil {
		return EpochInfo{}, err
	}
	blockHash, err := d.blockHashForEpoch(ctx, epoch)
	if err != nil {
		return EpochInfo{}, err
	}
	return EpochInfo{ActiveStake: total, BlockHash: blockHash, Nonce: nonce}, nil
}

func (d *dbDataSource) GetPoolStakeForEpoch(ctx context.Context, epoch int, poolIDBech32 string) (uint64, error) {
	return d.poolStakeForEpochRow(ctx, epoch, poolIDBech32)
}

// GetStakeSnapshots serves the Go/Set/Mark snapshots from the epoch_stake /
// epoch_param rows at E-1 / E / E+1 (E = current epoch by wall clock).
//
// The Mark entry's Nonce is empty until the E+1 nonce row exists (stake leads
// the nonce by one epoch); main.go takes the next-epoch nonce from
// GetCandidateNonce, so this only affects the informational fields.
func (d *dbDataSource) GetStakeSnapshots(ctx context.Context, poolIDBech32 string) (map[string]PoolStakeSnapshot, error) {
	current := int(CurrentEpoch())
	m := make(map[string]PoolStakeSnapshot, 3)
	for _, e := range []struct {
		kind  string
		epoch int
	}{
		{"Go", current - 1},
		{"Set", current},
		{"Mark", current + 1},
	} {
		nonce, found, err := d.nonceForEpoch(ctx, e.epoch)
		if err != nil {
			return nil, err
		}
		if !found && e.kind != "Mark" {
			return nil, fmt.Errorf("no nonce for epoch %d (the %s snapshot) in the cexplorer database — is the db-sync behind?", e.epoch, e.kind)
		}
		total, err := d.totalActiveStakeForEpoch(ctx, e.epoch)
		if err != nil {
			return nil, err
		}
		pool, err := d.poolStakeForEpochRow(ctx, e.epoch, poolIDBech32)
		if err != nil {
			return nil, err
		}
		m[e.kind] = PoolStakeSnapshot{
			EpochNo:     e.epoch,
			ActiveStake: total,
			PoolStake:   pool,
			Nonce:       nonce,
		}
	}
	return m, nil
}

// GetCandidateNonce is query (a) at E+1: "" while the upcoming epoch's nonce
// row is absent (which, in this database, is until the epoch actually starts).
func (d *dbDataSource) GetCandidateNonce(ctx context.Context) (string, error) {
	nonce, _, err := d.nonceForEpoch(ctx, int(CurrentEpoch())+1)
	return nonce, err
}
