package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	AccessibilityQuotaWindow = 24 * time.Hour
	MaxAccessibilityQuotas   = 4096
)

var ErrAccessibilityQuota = errors.New("store: accessibility issuance quota unavailable")

type AccessibilityQuotaError struct {
	RetryAfter time.Duration
}

func (e *AccessibilityQuotaError) Error() string { return ErrAccessibilityQuota.Error() }
func (e *AccessibilityQuotaError) Unwrap() error { return ErrAccessibilityQuota }

// Source aggregation stays out of the ledger; the pepper makes stored pseudonyms unusable across deployments.
func (d *DB) accessibilitySourceMAC(source string) []byte {
	m := hmac.New(sha256.New, d.pepper)
	_, _ = m.Write([]byte("epocha/v1|accessibility-quota|"))
	_, _ = m.Write([]byte(source))
	return m.Sum(nil)
}

// AccessibilityRetryAfter is advisory; reserveAccessibilityTx is the authoritative concurrent check.
func (d *DB) AccessibilityRetryAfter(ctx context.Context, siteID int64, source string, now time.Time) (time.Duration, error) {
	if siteID < 1 || source == "" || len(source) > 128 {
		return 0, ErrBadConfig
	}
	var own sql.NullString
	var earliest sql.NullString
	var count int
	err := d.h.QueryRowContext(ctx, `SELECT
(SELECT expires_at FROM accessibility_quotas WHERE site_id=? AND source_mac=? AND expires_at>?),
count(*), min(expires_at) FROM accessibility_quotas WHERE expires_at>?`,
		siteID, d.accessibilitySourceMAC(source), formatTime(now.UTC()), formatTime(now.UTC())).Scan(&own, &count, &earliest)
	if err != nil {
		return 0, fmt.Errorf("store: check accessibility quota: %w", err)
	}
	deadline := own
	if !deadline.Valid && count >= MaxAccessibilityQuotas {
		deadline = earliest
	}
	if !deadline.Valid {
		return 0, nil
	}
	until, err := parseTime(deadline.String)
	if err != nil {
		return 0, err
	}
	return until.Sub(now.UTC()), nil
}

func (d *DB) reserveAccessibilityTx(ctx context.Context, tx *sql.Tx, c *Challenge, source string) error {
	if source == "" || len(source) > 128 {
		return fmt.Errorf("%w: accessibility source", ErrBadConfig)
	}
	now := formatTime(c.CreatedAt.UTC())
	if _, err := tx.ExecContext(ctx, `DELETE FROM accessibility_quotas WHERE expires_at<=?`, now); err != nil {
		return fmt.Errorf("store: clean accessibility quotas: %w", err)
	}
	digest := d.accessibilitySourceMAC(source)
	res, err := tx.ExecContext(ctx, `
INSERT INTO accessibility_quotas(site_id, source_mac, expires_at)
SELECT ?, ?, ? WHERE
    EXISTS(SELECT 1 FROM accessibility_quotas WHERE site_id=? AND source_mac=?)
    OR (SELECT count(*) FROM accessibility_quotas) < ?
ON CONFLICT(site_id, source_mac) DO UPDATE SET expires_at=excluded.expires_at
WHERE accessibility_quotas.expires_at<=?`,
		c.SiteID, digest, formatTime(c.CreatedAt.UTC().Add(AccessibilityQuotaWindow)),
		c.SiteID, digest, MaxAccessibilityQuotas, now)
	if err != nil {
		return fmt.Errorf("store: reserve accessibility quota: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: count accessibility reservation: %w", err)
	}
	if n == 1 {
		return nil
	}
	var deadline string
	err = tx.QueryRowContext(ctx, `SELECT expires_at FROM accessibility_quotas WHERE site_id=? AND source_mac=?`, c.SiteID, digest).Scan(&deadline)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `SELECT min(expires_at) FROM accessibility_quotas`).Scan(&deadline)
	}
	if err != nil {
		return fmt.Errorf("store: read accessibility quota deadline: %w", err)
	}
	until, err := parseTime(deadline)
	if err != nil {
		return err
	}
	return &AccessibilityQuotaError{RetryAfter: until.Sub(c.CreatedAt.UTC())}
}
