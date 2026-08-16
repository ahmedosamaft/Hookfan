package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when a unique constraint rejects a write, e.g. a
// duplicate listener slug.
var ErrConflict = errors.New("conflict")

type Listener struct {
	ID                   int64     `json:"id"`
	Name                 string    `json:"name"`
	Slug                 string    `json:"slug"`
	Provider             string    `json:"provider"`
	VerificationMode     string    `json:"verification_mode"`
	SignatureHeader      string    `json:"signature_header"`
	SignaturePrefix      string    `json:"signature_prefix"`
	ChallengeVerifyToken string    `json:"challenge_verify_token,omitempty"`
	RoutingKeyPath       string    `json:"routing_key_path"`
	Enabled              bool      `json:"enabled"`
	CreatedAt            time.Time `json:"created_at"`
	HasSecret            bool      `json:"has_secret"`
	// Secret holds the decrypted signing secret. It is populated only by the
	// ingest lookup and is never serialised — note the json:"-" tag.
	Secret []byte `json:"-"`
}

const listenerColumns = `
	id, name, slug, provider, verification_mode, signature_header,
	signature_prefix, COALESCE(challenge_verify_token, ''), routing_key_path,
	enabled, created_at, secret`

// scanListener reads a row in listenerColumns order. The raw secret is returned
// separately so the caller decides whether to decrypt it.
func scanListener(row pgx.Row) (*Listener, []byte, error) {
	var l Listener
	var encSecret []byte
	err := row.Scan(&l.ID, &l.Name, &l.Slug, &l.Provider, &l.VerificationMode,
		&l.SignatureHeader, &l.SignaturePrefix, &l.ChallengeVerifyToken,
		&l.RoutingKeyPath, &l.Enabled, &l.CreatedAt, &encSecret)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	l.HasSecret = len(encSecret) > 0
	return &l, encSecret, nil
}

// ListenerBySlug loads the listener an inbound webhook is addressed to,
// returning the still-encrypted secret for the caller to decrypt.
func (s *Store) ListenerBySlug(ctx context.Context, slug string) (*Listener, []byte, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+listenerColumns+` FROM listeners WHERE slug = $1`, slug)
	return scanListener(row)
}

func (s *Store) ListenerByID(ctx context.Context, id int64) (*Listener, []byte, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+listenerColumns+` FROM listeners WHERE id = $1`, id)
	return scanListener(row)
}

func (s *Store) ListListeners(ctx context.Context) ([]*Listener, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+listenerColumns+` FROM listeners ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Listener
	for rows.Next() {
		l, _, err := scanListener(rows)
		if err != nil {
			return nil, err
		}
		// Never leak the verify token in a list response.
		l.ChallengeVerifyToken = ""
		out = append(out, l)
	}
	return out, rows.Err()
}

// CreateListenerParams carries the values needed to insert a listener. Secret
// is already encrypted by the caller.
type CreateListenerParams struct {
	Name                 string
	Slug                 string
	Provider             string
	VerificationMode     string
	SignatureHeader      string
	SignaturePrefix      string
	Secret               []byte
	ChallengeVerifyToken string
	RoutingKeyPath       string
	Enabled              bool
}

func (s *Store) CreateListener(ctx context.Context, p CreateListenerParams) (*Listener, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO listeners (
			name, slug, provider, verification_mode, signature_header,
			signature_prefix, secret, challenge_verify_token, routing_key_path, enabled)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING `+listenerColumns,
		p.Name, p.Slug, p.Provider, p.VerificationMode, p.SignatureHeader,
		p.SignaturePrefix, p.Secret, nullIfEmpty(p.ChallengeVerifyToken),
		p.RoutingKeyPath, p.Enabled)

	l, _, err := scanListener(row)
	if err != nil {
		return nil, wrapUnique(err, "slug")
	}
	return l, nil
}

// UpdateListenerParams holds the mutable listener fields. A nil pointer leaves
// the column untouched. The slug is immutable and so is absent.
type UpdateListenerParams struct {
	Name                 *string
	Provider             *string
	VerificationMode     *string
	SignatureHeader      *string
	SignaturePrefix      *string
	Secret               []byte
	ChallengeVerifyToken *string
	RoutingKeyPath       *string
	Enabled              *bool
}

func (s *Store) UpdateListener(ctx context.Context, id int64, p UpdateListenerParams) (*Listener, error) {
	// COALESCE keeps each column unchanged when its parameter is NULL, so one
	// statement serves any subset of fields.
	row := s.pool.QueryRow(ctx, `
		UPDATE listeners SET
			name                   = COALESCE($2, name),
			provider               = COALESCE($3, provider),
			verification_mode      = COALESCE($4, verification_mode),
			signature_header       = COALESCE($5, signature_header),
			signature_prefix       = COALESCE($6, signature_prefix),
			secret                 = COALESCE($7, secret),
			challenge_verify_token = COALESCE($8, challenge_verify_token),
			routing_key_path       = COALESCE($9, routing_key_path),
			enabled                = COALESCE($10, enabled)
		WHERE id = $1
		RETURNING `+listenerColumns,
		id, p.Name, p.Provider, p.VerificationMode, p.SignatureHeader,
		p.SignaturePrefix, p.Secret, p.ChallengeVerifyToken, p.RoutingKeyPath, p.Enabled)

	l, _, err := scanListener(row)
	return l, err
}

func (s *Store) DeleteListener(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM listeners WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// wrapUnique converts a Postgres unique-violation into ErrConflict so handlers
// can map it to 409 without inspecting driver errors themselves.
func wrapUnique(err error, field string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: %s already exists", ErrConflict, field)
	}
	return err
}
