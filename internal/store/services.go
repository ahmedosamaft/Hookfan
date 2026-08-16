package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type Service struct {
	ID       int64  `json:"-"`
	PublicID string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Method   string `json:"method"`
	Status   string `json:"status"`

	VerifiedAt      *time.Time `json:"verified_at,omitempty"`
	LastVerifyError string     `json:"last_verify_error,omitempty"`

	TimeoutMS     int               `json:"timeout_ms"`
	MaxAttempts   int               `json:"max_attempts"`
	RateLimitRPS  int               `json:"rate_limit_rps"`
	CustomHeaders map[string]string `json:"custom_headers"`

	ConsecutiveFailures int        `json:"consecutive_failures"`
	DisabledAt          *time.Time `json:"disabled_at,omitempty"`
	DisabledReason      string     `json:"disabled_reason,omitempty"`

	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`

	// LinkToken holds the decrypted token. It is never serialised: the
	// plaintext is shown exactly once, at create and at rotate, via a separate
	// response shape.
	LinkToken []byte `json:"-"`
}

const serviceColumns = `
	id, public_id, name, url, method, status, verified_at,
	COALESCE(last_verify_error, ''), timeout_ms, max_attempts, rate_limit_rps,
	custom_headers, consecutive_failures, disabled_at,
	COALESCE(disabled_reason, ''), enabled, created_at, link_token`

func scanService(row pgx.Row) (*Service, []byte, error) {
	var s Service
	var encToken []byte
	err := row.Scan(&s.ID, &s.PublicID, &s.Name, &s.URL, &s.Method, &s.Status,
		&s.VerifiedAt, &s.LastVerifyError, &s.TimeoutMS, &s.MaxAttempts,
		&s.RateLimitRPS, &s.CustomHeaders, &s.ConsecutiveFailures, &s.DisabledAt,
		&s.DisabledReason, &s.Enabled, &s.CreatedAt, &encToken)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	return &s, encToken, nil
}

func (s *Store) ServiceByPublicID(ctx context.Context, publicID string) (*Service, []byte, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+serviceColumns+` FROM services WHERE public_id = $1`, publicID)
	return scanService(row)
}

func (s *Store) ServiceByID(ctx context.Context, id int64) (*Service, []byte, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+serviceColumns+` FROM services WHERE id = $1`, id)
	return scanService(row)
}

func (s *Store) ListServices(ctx context.Context) ([]*Service, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+serviceColumns+` FROM services ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Service
	for rows.Next() {
		svc, _, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

type CreateServiceParams struct {
	PublicID      string
	Name          string
	URL           string
	Method        string
	LinkToken     []byte // already encrypted
	TimeoutMS     int
	MaxAttempts   int
	RateLimitRPS  int
	CustomHeaders map[string]string
	Enabled       bool
}

func (s *Store) CreateService(ctx context.Context, p CreateServiceParams) (*Service, error) {
	headers := p.CustomHeaders
	if headers == nil {
		headers = map[string]string{}
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO services (
			public_id, name, url, method, link_token, timeout_ms, max_attempts,
			rate_limit_rps, custom_headers, enabled)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING `+serviceColumns,
		p.PublicID, p.Name, p.URL, p.Method, p.LinkToken, p.TimeoutMS,
		p.MaxAttempts, p.RateLimitRPS, headers, p.Enabled)

	svc, _, err := scanService(row)
	if err != nil {
		return nil, wrapUnique(err, "service")
	}
	return svc, nil
}

type UpdateServiceParams struct {
	Name          *string
	URL           *string
	Method        *string
	TimeoutMS     *int
	MaxAttempts   *int
	RateLimitRPS  *int
	CustomHeaders map[string]string
	Enabled       *bool
	// ResetBreaker clears the circuit breaker, which is how an operator
	// re-enables a service that tripped. Re-enabling must be explicit.
	ResetBreaker bool
}

func (s *Store) UpdateService(ctx context.Context, id int64, p UpdateServiceParams) (*Service, error) {
	var headers any
	if p.CustomHeaders != nil {
		headers = p.CustomHeaders
	}

	// Changing the URL invalidates a previous verification: the token was
	// proven against the old address, not the new one.
	row := s.pool.QueryRow(ctx, `
		UPDATE services SET
			name           = COALESCE($2, name),
			url            = COALESCE($3, url),
			method         = COALESCE($4, method),
			timeout_ms     = COALESCE($5, timeout_ms),
			max_attempts   = COALESCE($6, max_attempts),
			rate_limit_rps = COALESCE($7, rate_limit_rps),
			custom_headers = COALESCE($8, custom_headers),
			enabled        = COALESCE($9, enabled),
			status = CASE
				WHEN $3::text IS NOT NULL AND $3::text <> url THEN 'pending'
				WHEN $10::boolean THEN 'pending'
				ELSE status END,
			verified_at = CASE
				WHEN $3::text IS NOT NULL AND $3::text <> url THEN NULL
				ELSE verified_at END,
			consecutive_failures = CASE WHEN $10::boolean THEN 0 ELSE consecutive_failures END,
			disabled_at     = CASE WHEN $10::boolean THEN NULL ELSE disabled_at END,
			disabled_reason = CASE WHEN $10::boolean THEN NULL ELSE disabled_reason END
		WHERE id = $1
		RETURNING `+serviceColumns,
		id, p.Name, p.URL, p.Method, p.TimeoutMS, p.MaxAttempts, p.RateLimitRPS,
		headers, p.Enabled, p.ResetBreaker)

	svc, _, err := scanService(row)
	return svc, err
}

func (s *Store) DeleteService(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM services WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RotateLinkToken replaces the token and returns the service to `pending`: the
// new token has not yet been proven against the receiving backend.
func (s *Store) RotateLinkToken(ctx context.Context, id int64, encToken []byte) (*Service, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE services
		   SET link_token = $2, status = 'pending', verified_at = NULL,
		       last_verify_error = NULL
		 WHERE id = $1
		RETURNING `+serviceColumns, id, encToken)
	svc, _, err := scanService(row)
	return svc, err
}

// MarkVerified records a successful link handshake.
func (s *Store) MarkVerified(ctx context.Context, id int64) (*Service, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE services
		   SET status = 'verified', verified_at = now(), last_verify_error = NULL,
		       consecutive_failures = 0, disabled_at = NULL, disabled_reason = NULL
		 WHERE id = $1
		RETURNING `+serviceColumns, id)
	svc, _, err := scanService(row)
	return svc, err
}

// MarkVerifyFailed records why a handshake failed, so the UI can show the
// exact reason rather than a generic failure.
func (s *Store) MarkVerifyFailed(ctx context.Context, id int64, reason string) (*Service, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE services
		   SET status = 'failed', last_verify_error = $2
		 WHERE id = $1
		RETURNING `+serviceColumns, id, reason)
	svc, _, err := scanService(row)
	return svc, err
}
