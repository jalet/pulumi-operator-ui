package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	// Bounds keep one hostile or misconfigured IdP token from bloating the audit table.
	authClaimValuesMax     = 50
	authClaimValueBytesMax = 256
	authDetailBytesMax     = 1024
)

// AuthOutcome is the result of a login attempt.
type AuthOutcome string

// Auth outcomes.
const (
	AuthOutcomeLogin  AuthOutcome = "login"
	AuthOutcomeDenied AuthOutcome = "denied"
	AuthOutcomeError  AuthOutcome = "error"
)

// AuthEvent is one row of the auth_events audit table.
type AuthEvent struct {
	At          time.Time
	Subject     string
	Email       string
	Outcome     AuthOutcome
	ClaimValues []string
	Detail      string
}

// InsertAuthEvent appends an audit record, truncating oversized fields.
func (s *Store) InsertAuthEvent(ctx context.Context, e AuthEvent) error {
	assert(e.Outcome != "", "auth outcome")
	values := e.ClaimValues[:min(len(e.ClaimValues), authClaimValuesMax)]
	capped := make([]string, len(values))
	for i, v := range values {
		capped[i] = truncate(v, authClaimValueBytesMax)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO auth_events (at, subject, email, outcome, claim_values, detail)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		e.At, truncate(e.Subject, authClaimValueBytesMax), truncate(e.Email, authClaimValueBytesMax),
		e.Outcome, capped, truncate(e.Detail, authDetailBytesMax))
	if err != nil {
		return fmt.Errorf("insert auth event: %w", err)
	}
	return nil
}

// truncate cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
