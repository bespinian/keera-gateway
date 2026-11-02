package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// RecurringCharge is a fee an operator charges an organisation every month or
// year, such as for a dedicated model. Each period is charged to the saved
// card when it starts, and is not credited. See docs/billing.md.
type RecurringCharge struct {
	ID           string `json:"id"`
	OrgID        string `json:"org_id"`
	Name         string `json:"name"`
	AmountMicros int64  `json:"amount_micros"`
	// Every is "month" or "year".
	Every string `json:"every"`
	// StartsOn is the day the first period starts, as YYYY-MM-DD in UTC.
	StartsOn    string `json:"starts_on"`
	PaidPeriods int    `json:"paid_periods"`
	// DueAt is when the first period not paid starts, and DueUntil when it
	// ends.
	DueAt    time.Time `json:"due_at"`
	DueUntil time.Time `json:"due_until"`
	// Error is why charging the saved card last failed. Until it is cleared,
	// the card is not tried again.
	Error     string    `json:"error,omitempty"`
	Actor     string    `json:"actor"`
	CreatedAt time.Time `json:"created_at"`
}

// Charge periods are counted from starts_on, so a charge from the 31st is due
// on the last day of shorter months and on the 31st again after. Midnight UTC,
// like budgets.
const (
	chargeDue = `((c.starts_on + c.paid_periods * ('1 ' || c.every)::interval)
		AT TIME ZONE 'UTC')`
	chargeDueUntil = `((c.starts_on + (c.paid_periods + 1) * ('1 ' || c.every)::interval)
		AT TIME ZONE 'UTC')`
	chargeColumns = `c.id, c.org_id, c.name, c.micros, c.every, c.starts_on, c.paid_periods, ` +
		chargeDue + `, ` + chargeDueUntil + `, c.error, c.actor, c.created_at`
)

func scanCharge(r row) (RecurringCharge, error) {
	var (
		c      RecurringCharge
		starts time.Time
		msg    *string
	)
	err := r.Scan(&c.ID, &c.OrgID, &c.Name, &c.AmountMicros, &c.Every, &starts, &c.PaidPeriods,
		&c.DueAt, &c.DueUntil, &msg, &c.Actor, &c.CreatedAt)
	c.StartsOn, c.Error = starts.Format(time.DateOnly), deref(msg)
	return c, err
}

// CreateCharge adds a recurring charge. It also opens the organisation's
// credit account, which the gateway reads to know whether a charge is due.
func (s *Store) CreateCharge(ctx context.Context, c RecurringCharge) (out RecurringCharge, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO credit_accounts (org_id) VALUES ($1)
			ON CONFLICT (org_id) DO NOTHING`, c.OrgID); err != nil {
			return err
		}
		out, err = scanCharge(tx.QueryRow(ctx, `
			WITH c AS (INSERT INTO recurring_charges (id, org_id, name, micros, every, starts_on, actor)
				VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING *)
			SELECT `+chargeColumns+` FROM c`,
			c.ID, c.OrgID, c.Name, c.AmountMicros, c.Every, c.StartsOn, c.Actor))
		return err
	})
	if IsMissingOrg(err) {
		return RecurringCharge{}, ErrNotFound
	}
	return out, err
}

// ListCharges lists an organisation's recurring charges, or with an empty
// orgID every one's, the first due first.
func (s *Store) ListCharges(ctx context.Context, orgID string) ([]RecurringCharge, error) {
	return queryAll(ctx, s.pool, scanCharge, `SELECT `+chargeColumns+` FROM recurring_charges c
		WHERE $1 = '' OR c.org_id = $1 ORDER BY 8, c.id`, orgID)
}

func (s *Store) Charge(ctx context.Context, id string) (RecurringCharge, error) {
	c, err := scanCharge(s.pool.QueryRow(ctx,
		`SELECT `+chargeColumns+` FROM recurring_charges c WHERE c.id = $1`, id))
	return c, notFound(err)
}

// DeleteCharge ends a recurring charge. Its payments are kept, and a period
// not paid by then is no longer due.
func (s *Store) DeleteCharge(ctx context.Context, id string) (RecurringCharge, error) {
	c, err := scanCharge(s.pool.QueryRow(ctx, `
		WITH c AS (DELETE FROM recurring_charges WHERE id = $1 RETURNING *)
		SELECT `+chargeColumns+` FROM c`, id))
	return c, notFound(err)
}

// DueCharge is a recurring charge whose period has started, and the saved
// card to charge it to.
type DueCharge struct {
	RecurringCharge
	CardToken int64
}

// DueCharges lists the recurring charges due now that the saved card can pay:
// there is a card, it was not declined, and no charge of it is under way or
// was tried in the last ten minutes. One paid on the payment page in the last
// hour is not charged to the card as well.
func (s *Store) DueCharges(ctx context.Context, limit int) ([]DueCharge, error) {
	return queryAll(ctx, s.pool, func(r row) (d DueCharge, err error) {
		d.RecurringCharge, err = scanCharge(withTail{r, &d.CardToken})
		return d, err
	}, `
		SELECT `+chargeColumns+`, a.card_token
		FROM recurring_charges c
		JOIN credit_accounts a ON a.org_id = c.org_id
		WHERE `+chargeIsDue+`
		ORDER BY 8 LIMIT $1`, limit)
}

// chargeIsDue holds for a recurring charge c, with its account a, that
// DueCharges lists. CreatePayment checks it again, as the list may be stale.
const chargeIsDue = `a.card_token IS NOT NULL AND c.error IS NULL AND ` + chargeDue + ` <= now()
	AND NOT EXISTS (SELECT 1 FROM payments p WHERE p.org_id = c.org_id
	                AND p.charge_id = c.id AND p.state = 'pending'
	                AND (NOT p.save_card OR p.ts > now() - interval '1 hour'))
	AND NOT EXISTS (SELECT 1 FROM payments p WHERE p.org_id = c.org_id
	                AND p.charge_id = c.id AND NOT p.save_card
	                AND p.ts > now() - interval '10 minutes')`

// payPeriod counts a charge's period as paid. It reports whether that period
// was paid already, by another payment: the organisation then paid twice.
func payPeriod(ctx context.Context, tx pgx.Tx, p Payment) (twice bool, err error) {
	tag, err := tx.Exec(ctx, `UPDATE recurring_charges c SET paid_periods = paid_periods + 1
		WHERE c.id = $1 AND `+chargeDue+` = $2`, p.ChargeID, p.PeriodStart)
	if err != nil || tag.RowsAffected() > 0 {
		return false, err
	}
	// A charge removed since is not a second payment.
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM recurring_charges WHERE id = $1)`,
		p.ChargeID).Scan(&twice)
	return twice, err
}

// withTail scans a row's leading columns into what Scan is given, and the
// last into tail.
type withTail struct {
	row
	tail any
}

func (w withTail) Scan(dest ...any) error { return w.row.Scan(append(dest, w.tail)...) }
