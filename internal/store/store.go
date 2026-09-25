// Package store persists stacks, runs and auth events in PostgreSQL.
package store

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
	"github.com/rs/zerolog"

	"github.com/jalet/pulumi-operator-ui/internal/events"
)

// The database may start after the app (CNPG failover, fresh install). 30 attempts with
// backoff capped at 10s gives roughly four minutes before the pod restarts. A variable so
// tests can fail fast.
var connectAttemptsMax = 30

const (
	connectBackoffFirst = 250 * time.Millisecond
	connectBackoffCap   = 10 * time.Second
	poolConnsMax        = 10
)

//go:embed migrations/*.sql
var _migrations embed.FS

// RunType is the kind of Pulumi operation.
type RunType string

// RunState is the lifecycle state of a run.
type RunState string

// CommitSource says where a run's commit came from.
type CommitSource string

// Run types, states and commit sources. The empty string is never a valid type or state.
const (
	RunTypePreview RunType = "preview"
	RunTypeUp      RunType = "up"
	RunTypeRefresh RunType = "refresh"
	RunTypeDestroy RunType = "destroy"

	RunStatePending   RunState = "pending"
	RunStateRunning   RunState = "running"
	RunStateSucceeded RunState = "succeeded"
	RunStateFailed    RunState = "failed"

	CommitSourceUpdate CommitSource = "update"
	CommitSourceStack  CommitSource = "stack"
)

// Stack is one row of the stacks table.
type Stack struct {
	Namespace   string
	Name        string
	Ready       bool
	Reconciling bool
	Stalled     bool
	LastCommit  string
	UpdatedAt   time.Time
	DeletedAt   *time.Time
}

// Run is one row of the runs table.
type Run struct {
	ID           int64
	Namespace    string
	UpdateName   string
	UID          string // "" when backfilled from Stack.status.lastUpdate
	StackName    string
	Type         RunType
	Commit       string
	CommitSource CommitSource
	State        RunState
	Message      string
	StartedAt    *time.Time
	EndedAt      *time.Time
	ObservedAt   time.Time
}

// Publisher receives a notification after each committed change.
type Publisher interface {
	Publish(e events.Event)
}

// ErrNotFound is returned by reads for a missing row.
var ErrNotFound = errors.New("not found")

// Store is the PostgreSQL-backed persistence layer.
type Store struct {
	pool *pgxpool.Pool
	pub  Publisher
	log  zerolog.Logger
}

// Options configures Open.
type Options struct {
	URL    string
	CAFile string // "" = system roots
	Pub    Publisher
	Log    zerolog.Logger
}

// Open connects with retry, applies migrations and returns a ready store.
func Open(ctx context.Context, o Options) (*Store, error) {
	assert(o.Pub != nil, "store publisher")
	cfg, err := pgxpool.ParseConfig(o.URL)
	if err != nil {
		return nil, errors.New("parse database url: invalid") // never echo the URL
	}
	if o.CAFile != "" {
		if err := requireVerifiedTLS(cfg, o.CAFile); err != nil {
			return nil, err
		}
	}
	cfg.MaxConns = poolConnsMax
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("new pool: %w", err)
	}
	if err := waitForDatabase(ctx, pool, o.Log); err != nil {
		pool.Close()
		return nil, err
	}
	if err := migrate(ctx, cfg); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool, pub: o.Pub, log: o.Log}, nil
}

// Close releases the connection pool.
func (s *Store) Close() { s.pool.Close() }

// Ping checks that the database answers.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

// requireVerifiedTLS makes every connection attempt use TLS verified against caFile.
// pgx's default sslmode=prefer adds a plaintext fallback that is tried when TLS fails,
// including on a verification error, so those fallbacks are dropped.
func requireVerifiedTLS(cfg *pgxpool.Config, caFile string) error {
	pem, err := os.ReadFile(caFile) //nolint:gosec // path is operator configuration, not user input
	if err != nil {
		return fmt.Errorf("read database CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return errors.New("read database CA: no certificates found")
	}
	verified := func(host string) *tls.Config {
		return &tls.Config{RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12}
	}
	cc := cfg.ConnConfig
	cc.TLSConfig = verified(cc.Host)
	kept := cc.Fallbacks[:0]
	for _, fb := range cc.Fallbacks {
		if fb.TLSConfig == nil {
			continue
		}
		fb.TLSConfig = verified(fb.Host)
		kept = append(kept, fb)
	}
	cc.Fallbacks = kept
	return nil
}

func waitForDatabase(ctx context.Context, pool *pgxpool.Pool, log zerolog.Logger) error {
	backoff := connectBackoffFirst
	var lastErr error
	for attempt := 1; attempt <= connectAttemptsMax; attempt++ {
		lastErr = pool.Ping(ctx)
		if lastErr == nil {
			return nil
		}
		log.Warn().Err(lastErr).Int("attempt", attempt).Msg("database not ready")
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("connect database: %w", ctx.Err())
		case <-timer.C:
		}
		backoff = min(backoff*2, connectBackoffCap)
	}
	return fmt.Errorf("connect database: gave up after %d attempts: %w", connectAttemptsMax, lastErr)
}

func migrate(ctx context.Context, cfg *pgxpool.Config) error {
	db := stdlib.OpenDB(*cfg.ConnConfig)
	defer func() { _ = db.Close() }() // read-only handle for migrations; close error is moot
	sub, err := fs.Sub(_migrations, "migrations")
	if err != nil {
		panic("invariant violated: embedded migrations: " + err.Error())
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("migration locker: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, sub, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("migration provider: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

func assert(cond bool, msg string) {
	if !cond {
		panic("invariant violated: " + msg)
	}
}
