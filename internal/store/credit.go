package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// CreditAccount is an organisation's credit for the deployment's provider
// keys, in CHF. See docs/billing.md.
type CreditAccount struct {
	OrgID         string `json:"org_id"`
	BalanceMicros int64  `json:"balance_micros"`
	// The automatic top-up: below TopUpBelowMicros, the saved card is charged
	// TopUpMicros. Zero is off.
	TopUpBelowMicros int64 `json:"topup_below_micros"`
	TopUpMicros      int64 `json:"topup_micros"`
	// CardToken is PostFinance's token for the saved card. Card is how it is
	// shown, nil without one.
	CardToken int64      `json:"-"`
	Card      *SavedCard `json:"card,omitempty"`
	// TopUpError is why the last automatic top-up failed. Until it is
	// cleared, no other is tried.
	TopUpError string `json:"topup_error,omitempty"`
	// Invoiced lets the organisation use the keys without credit.
	Invoiced bool `json:"invoiced"`
}

// SavedCard is how a saved card is shown.
type SavedCard struct {
	Label   string     `json:"label"`
	Expires *time.Time `json:"expires,omitempty"`
}

const creditColumns = `org_id, balance_micros, topup_below_micros, topup_micros,
	card_token, card_label, card_expires, topup_error, invoiced`

func scanCredit(r row) (CreditAccount, error) {
	var (
		a       CreditAccount
		token   *int64
		label   *string
		expires *time.Time
		topUp   *string
	)
	err := r.Scan(&a.OrgID, &a.BalanceMicros, &a.TopUpBelowMicros, &a.TopUpMicros,
		&token, &label, &expires, &topUp, &a.Invoiced)
	if token != nil {
		a.CardToken = *token
		a.Card = &SavedCard{Expires: expires}
		if label != nil {
			a.Card.Label = *label
		}
	}
	if topUp != nil {
		a.TopUpError = *topUp
	}
	return a, err
}

// CreditAccount reads an organisation's credit. One that never had any has
// a zero balance.
func (s *Store) CreditAccount(ctx context.Context, orgID string) (CreditAccount, error) {
	a, err := scanCredit(s.pool.QueryRow(ctx,
		`SELECT `+creditColumns+` FROM credit_accounts WHERE org_id = $1`, orgID))
	if errors.Is(err, pgx.ErrNoRows) {
		return CreditAccount{OrgID: orgID}, nil
	}
	return a, err
}

// CreditRow is what the gateway needs of an account to admit a request.
type CreditRow struct {
	OrgID         string
	BalanceMicros int64
	Invoiced      bool
}

// LoadCredit reads every organisation's balance. An organisation without a
// row has none.
func (s *Store) LoadCredit(ctx context.Context) ([]CreditRow, error) {
	return queryAll(ctx, s.pool, scanCreditRow, `SELECT org_id, balance_micros, invoiced FROM credit_accounts`)
}

func scanCreditRow(r row) (CreditRow, error) {
	var c CreditRow
	err := r.Scan(&c.OrgID, &c.BalanceMicros, &c.Invoiced)
	return c, err
}

// CreditSettings changes an account. A nil field is left as it is.
type CreditSettings struct {
	TopUpBelowMicros *int64
	TopUpMicros      *int64
	Invoiced         *bool
}

// UpdateCredit changes an account's settings. Changing the automatic top-up
// clears the reason the last one failed, so it is tried again.
func (s *Store) UpdateCredit(ctx context.Context, orgID string, c CreditSettings) (CreditAccount, error) {
	topUp := c.TopUpBelowMicros != nil || c.TopUpMicros != nil
	return scanCredit(s.pool.QueryRow(ctx, `
		INSERT INTO credit_accounts (org_id, topup_below_micros, topup_micros, invoiced)
		VALUES ($1, coalesce($2::bigint, 0), coalesce($3::bigint, 0), coalesce($4, false))
		ON CONFLICT (org_id) DO UPDATE SET
			topup_below_micros = coalesce($2::bigint, credit_accounts.topup_below_micros),
			topup_micros = coalesce($3::bigint, credit_accounts.topup_micros),
			invoiced = coalesce($4, credit_accounts.invoiced),
			topup_error = CASE WHEN $5 THEN NULL ELSE credit_accounts.topup_error END,
			updated_at = now()
		RETURNING `+creditColumns,
		orgID, c.TopUpBelowMicros, c.TopUpMicros, c.Invoiced, topUp))
}

// ForgetCard removes the saved card, and returns its token so PostFinance can
// forget it too. Zero when there was none. The automatic top-up goes with it,
// so saving the next card does not start it again unasked.
func (s *Store) ForgetCard(ctx context.Context, orgID string) (int64, error) {
	var token *int64
	err := s.pool.QueryRow(ctx, `
		UPDATE credit_accounts a SET card_token = NULL, card_label = NULL,
			card_expires = NULL, topup_below_micros = 0, topup_micros = 0, updated_at = now()
		FROM (SELECT org_id, card_token FROM credit_accounts WHERE org_id = $1) old
		WHERE a.org_id = old.org_id RETURNING old.card_token`, orgID).Scan(&token)
	if errors.Is(err, pgx.ErrNoRows) || token == nil {
		return 0, nil
	}
	return *token, err
}

// DueTopUps lists the accounts whose balance has fallen below their automatic
// top-up, which have a card to charge and no top-up under way or in the last
// ten minutes.
func (s *Store) DueTopUps(ctx context.Context, limit int) ([]CreditAccount, error) {
	return queryAll(ctx, s.pool, scanCredit, `
		SELECT a.org_id, a.balance_micros, a.topup_below_micros, a.topup_micros,
		       a.card_token, a.card_label, a.card_expires, a.topup_error, a.invoiced
		FROM credit_accounts a
		-- A deleted organisation's card is not charged.
		JOIN orgs o ON o.id = a.org_id
		WHERE a.card_token IS NOT NULL AND a.topup_micros > 0
		  AND a.balance_micros < a.topup_below_micros
		  AND a.topup_error IS NULL AND NOT a.invoiced
		  AND NOT EXISTS (SELECT 1 FROM payments p WHERE p.org_id = a.org_id
		                  AND p.kind = 'auto' AND p.state = 'pending')
		  -- At most one every ten minutes, whatever happened to the last: a
		  -- balance driven far below zero is not paid off by a run of charges.
		  AND NOT EXISTS (SELECT 1 FROM payments p WHERE p.org_id = a.org_id
		                  AND p.kind = 'auto' AND p.ts > now() - interval '10 minutes')
		ORDER BY a.org_id LIMIT $1`, limit)
}

// PaymentKind says where money came from.
type PaymentKind string

// The kinds of payment.
const (
	PaymentTopUp PaymentKind = "topup"
	PaymentAuto  PaymentKind = "auto"
	PaymentGrant PaymentKind = "grant"
)

// PaymentState is how far a payment got.
type PaymentState string

// The states of a payment.
const (
	PaymentPending PaymentState = "pending"
	PaymentPaid    PaymentState = "paid"
	PaymentFailed  PaymentState = "failed"
)

// Payment is money into a credit account.
type Payment struct {
	ID        string       `json:"id"`
	OrgID     string       `json:"org_id"`
	TS        time.Time    `json:"ts"`
	Kind      PaymentKind  `json:"kind"`
	Micros    int64        `json:"micros"`
	VATMicros int64        `json:"vat_micros"`
	State     PaymentState `json:"state"`
	SaveCard  bool         `json:"save_card,omitempty"`
	// TransactionID is PostFinance's, zero on a grant.
	TransactionID int64      `json:"transaction_id,omitempty"`
	Actor         string     `json:"actor"`
	Note          string     `json:"note,omitempty"`
	SettledAt     *time.Time `json:"settled_at,omitempty"`
}

const paymentColumns = `id, org_id, ts, kind, micros, vat_micros, state, save_card,
	transaction_id, actor, note, settled_at`

func scanPayment(r row) (Payment, error) {
	var (
		p    Payment
		tx   *int64
		note *string
	)
	err := r.Scan(&p.ID, &p.OrgID, &p.TS, &p.Kind, &p.Micros, &p.VATMicros, &p.State,
		&p.SaveCard, &tx, &p.Actor, &note, &p.SettledAt)
	if tx != nil {
		p.TransactionID = *tx
	}
	if note != nil {
		p.Note = *note
	}
	return p, err
}

// ErrTopUpUnderway is returned for an automatic top-up while another for the
// same organisation is still pending.
var ErrTopUpUnderway = errors.New("store: an automatic top-up is under way")

// CreatePayment records a payment that has not been paid yet.
func (s *Store) CreatePayment(ctx context.Context, p Payment) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO payments (id, org_id, kind, micros, vat_micros,
		state, save_card, actor) VALUES ($1,$2,$3,$4,$5,'pending',$6,$7)`,
		p.ID, p.OrgID, p.Kind, p.Micros, p.VATMicros, p.SaveCard, p.Actor)
	if uniqueOn(err, "payments_one_auto_idx") {
		return ErrTopUpUnderway
	}
	return err
}

// SetTransaction ties a pending payment to PostFinance's transaction.
func (s *Store) SetTransaction(ctx context.Context, paymentID string, transactionID int64) error {
	return s.execOne(ctx, `UPDATE payments SET transaction_id = $2
		WHERE id = $1 AND state = 'pending'`, paymentID, transactionID)
}

// Grant adds credit without a payment, or takes some away. It is paid at
// once.
func (s *Store) Grant(ctx context.Context, p Payment) (Payment, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Payment{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	out, err := scanPayment(tx.QueryRow(ctx, `INSERT INTO payments (id, org_id, kind, micros,
		state, actor, note, settled_at) VALUES ($1,$2,'grant',$3,'paid',$4,$5,now())
		RETURNING `+paymentColumns, p.ID, p.OrgID, p.Micros, p.Actor, nullable(p.Note)))
	if err != nil {
		return Payment{}, err
	}
	if err := addCredit(ctx, tx, p.OrgID, p.Micros); err != nil {
		return Payment{}, err
	}
	return out, tx.Commit(ctx)
}

func addCredit(ctx context.Context, tx pgx.Tx, orgID string, micros int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO credit_accounts (org_id, balance_micros) VALUES ($1, $2)
		ON CONFLICT (org_id) DO UPDATE
		SET balance_micros = credit_accounts.balance_micros + $2, updated_at = now()`,
		orgID, micros)
	return err
}

// Settlement is how a pending payment ended.
type Settlement struct {
	Paid bool
	// Note is why it failed.
	Note string
	// Card is the card the payment saved, with Token set, or nil.
	Card *SavedCard
	// Token is PostFinance's token for Card.
	Token int64
	// Retry says a failed automatic top-up failed for a reason of Keera's or
	// the network's, not the card's, so the next one is still tried.
	Retry bool
}

// Settle ends a pending payment. A paid one adds its credit, saves its card
// if it brought one, clears a failed top-up and lifts the organisation's
// limit; a failed automatic top-up stops the next. A failed payment that is
// paid after all, such as a top-up given up on after a day, is credited too.
// settled is false when the payment had ended already, so a webhook and a
// poll arriving together credit it once. oldToken is the card a newly saved
// one replaced, for PostFinance to forget.
func (s *Store) Settle(ctx context.Context, paymentID string, how Settlement) (
	p Payment, settled bool, oldToken int64, err error,
) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Payment{}, false, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	state := PaymentFailed
	if how.Paid {
		state = PaymentPaid
	}
	p, err = scanPayment(tx.QueryRow(ctx, `UPDATE payments SET state = $2, note = $3,
		settled_at = now() WHERE id = $1 AND (state = 'pending' OR (state = 'failed' AND $4))
		RETURNING `+paymentColumns,
		paymentID, state, nullable(how.Note), how.Paid))
	if errors.Is(err, pgx.ErrNoRows) {
		p, err = s.Payment(ctx, paymentID)
		return p, false, 0, err
	}
	if err != nil {
		return Payment{}, false, 0, err
	}
	switch {
	case how.Paid:
		if err := addCredit(ctx, tx, p.OrgID, p.Micros); err != nil {
			return Payment{}, false, 0, err
		}
		// Paying once lifts the limit a signed-up organisation starts with.
		// A grant does not: it is not money from the organisation.
		if _, err := tx.Exec(ctx, "UPDATE orgs SET limited = false WHERE id = $1 AND limited",
			p.OrgID); err != nil {
			return Payment{}, false, 0, err
		}
		var old *int64
		if err := tx.QueryRow(ctx, `UPDATE credit_accounts SET topup_error = NULL
			WHERE org_id = $1 RETURNING card_token`, p.OrgID).Scan(&old); err != nil {
			return Payment{}, false, 0, err
		}
		if how.Card != nil && how.Token != 0 {
			if _, err := tx.Exec(ctx, `UPDATE credit_accounts SET card_token = $2,
				card_label = $3, card_expires = $4 WHERE org_id = $1`,
				p.OrgID, how.Token, how.Card.Label, how.Card.Expires); err != nil {
				return Payment{}, false, 0, err
			}
			if old != nil && *old != how.Token {
				oldToken = *old
			}
		}
	case p.Kind == PaymentAuto && !how.Retry:
		if _, err := tx.Exec(ctx, `UPDATE credit_accounts SET topup_error = $2
			WHERE org_id = $1`, p.OrgID, how.Note); err != nil {
			return Payment{}, false, 0, err
		}
	}
	return p, true, oldToken, tx.Commit(ctx)
}

// Payment reads one payment.
func (s *Store) Payment(ctx context.Context, id string) (Payment, error) {
	p, err := scanPayment(s.pool.QueryRow(ctx,
		`SELECT `+paymentColumns+` FROM payments WHERE id = $1`, id))
	return p, notFound(err)
}

// PaymentByTransaction finds the payment of a PostFinance transaction.
func (s *Store) PaymentByTransaction(ctx context.Context, transactionID int64) (Payment, error) {
	p, err := scanPayment(s.pool.QueryRow(ctx,
		`SELECT `+paymentColumns+` FROM payments WHERE transaction_id = $1`, transactionID))
	return p, notFound(err)
}

// ListPayments lists an organisation's payments, newest first. An empty
// orgID lists every organisation's.
func (s *Store) ListPayments(ctx context.Context, orgID string, limit int) ([]Payment, error) {
	return queryAll(ctx, s.pool, scanPayment, `SELECT `+paymentColumns+` FROM payments
		WHERE ($1 = '' OR org_id = $1) ORDER BY ts DESC, id DESC LIMIT $2`,
		orgID, pageLimit(limit, 50, 500))
}

// PendingPayments lists the payments still pending that started between
// since and before, oldest first, and every automatic top-up still pending
// from before that.
func (s *Store) PendingPayments(ctx context.Context, since, before time.Time, limit int) ([]Payment, error) {
	return queryAll(ctx, s.pool, scanPayment, `SELECT `+paymentColumns+` FROM payments
		WHERE state = 'pending' AND ts < $2 AND (ts >= $1 OR kind = 'auto')
		ORDER BY ts LIMIT $3`, since, before, limit)
}
