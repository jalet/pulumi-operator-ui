package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/jalet/pulumi-operator-ui/internal/events"
)

// _adminURL points at the shared test container; each test gets its own database.
var _adminURL string

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	ctx := context.Background()
	ctr, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("pou"),
		postgres.WithUsername("pou"),
		postgres.WithPassword("pou"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start postgres:", err)
		return 1
	}
	defer func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			fmt.Fprintln(os.Stderr, "terminate postgres:", err)
		}
	}()
	_adminURL, err = ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, "connection string:", err)
		return 1
	}
	return m.Run()
}

// newDatabase creates an empty database and returns its URL.
func newDatabase(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	name := "t_" + hex.EncodeToString(b)
	conn, err := pgx.Connect(t.Context(), _adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }() // test cleanup only
	if _, err := conn.Exec(t.Context(), "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(_adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

func newTestStore(t *testing.T) (*Store, *recordingPublisher) {
	t.Helper()
	pub := &recordingPublisher{}
	s, err := Open(t.Context(), Options{URL: newDatabase(t), Pub: pub, Log: zerolog.Nop()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, pub
}

type recordingPublisher struct {
	mu     sync.Mutex
	events []events.Event
}

func (p *recordingPublisher) Publish(e events.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
}

func (p *recordingPublisher) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = nil
}

func (p *recordingPublisher) kinds() []events.Kind {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []events.Kind
	for _, e := range p.events {
		out = append(out, e.Kind)
	}
	return out
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func run(ns, name string, state RunState) Run {
	return Run{
		Namespace:  ns,
		UpdateName: name,
		UID:        "uid-" + name,
		StackName:  "s",
		Type:       RunTypeUp,
		State:      state,
		ObservedAt: time.Now(),
	}
}

func getRunByName(t *testing.T, s *Store, ns, name string) Run {
	t.Helper()
	var r Run
	var uid *string
	err := s.pool.QueryRow(t.Context(), `
		SELECT id, namespace, update_name, uid, stack_name, type, commit, commit_source,
		       state, message, started_at, ended_at, observed_at
		FROM runs WHERE namespace = $1 AND update_name = $2`, ns, name).Scan(
		&r.ID, &r.Namespace, &r.UpdateName, &uid, &r.StackName, &r.Type, &r.Commit,
		&r.CommitSource, &r.State, &r.Message, &r.StartedAt, &r.EndedAt, &r.ObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	if uid != nil {
		r.UID = *uid
	}
	return r
}

func countRuns(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	must(t, s.pool.QueryRow(t.Context(), `SELECT count(*) FROM runs`).Scan(&n))
	return n
}

func getStackRow(t *testing.T, s *Store, ns, name string) Stack {
	t.Helper()
	var st Stack
	err := s.pool.QueryRow(t.Context(), `
		SELECT namespace, name, ready, reconciling, stalled, last_commit, updated_at, deleted_at
		FROM stacks WHERE namespace = $1 AND name = $2`, ns, name).Scan(
		&st.Namespace, &st.Name, &st.Ready, &st.Reconciling, &st.Stalled, &st.LastCommit,
		&st.UpdatedAt, &st.DeletedAt)
	if err != nil {
		t.Fatal(err)
	}
	return st
}
