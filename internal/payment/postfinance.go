// Package payment takes the payments for prepaid credit, through PostFinance
// Checkout. See docs/billing.md.
//
// Card details never reach Keera Gateway: the customer types them on
// PostFinance's payment page, and what comes back is a transaction and,
// when asked for, a token to charge the same card again.
package payment

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultURL is PostFinance Checkout. A test space lives at the same address.
const DefaultURL = "https://checkout.postfinance.ch"

// Settings are the PostFinance Checkout account payments go to.
type Settings struct {
	// URL is DefaultURL, or a stand-in in tests.
	URL string
	// SpaceID is the space the transactions are created in.
	SpaceID int64
	// UserID and AuthKey are an application user of that space. AuthKey is
	// decoded already: PostFinance shows it in base64.
	UserID  string
	AuthKey []byte
}

// Client calls the PostFinance Checkout API, version 2.0.
type Client struct {
	s    Settings
	http *http.Client
}

// New builds a client.
func New(s Settings) *Client {
	s.URL = strings.TrimRight(s.URL, "/")
	if s.URL == "" {
		s.URL = DefaultURL
	}
	return &Client{s: s, http: &http.Client{Timeout: 30 * time.Second}}
}

// The ways a transaction can save the card for later charges.
const (
	// SaveCard keeps the card, and offers it on the payment page next time.
	SaveCard = "FORCE_CREATION_WITH_ONE_CLICK_PAYMENT"
)

// LineItem is what a transaction is for.
type LineItem struct {
	Name               string      `json:"name"`
	Quantity           int         `json:"quantity"`
	AmountIncludingTax json.Number `json:"amountIncludingTax"`
	Type               string      `json:"type"`
	UniqueID           string      `json:"uniqueId"`
	SKU                string      `json:"sku,omitempty"`
	Taxes              []Tax       `json:"taxes,omitempty"`
}

// Tax is one tax on a line item, such as VAT.
type Tax struct {
	Rate  json.Number `json:"rate"`
	Title string      `json:"title"`
}

// NewTransaction is a transaction to create.
type NewTransaction struct {
	Currency  string     `json:"currency"`
	LineItems []LineItem `json:"lineItems"`
	// CustomerID ties the transaction, and the card it saves, to an
	// organisation, so the payment page offers that organisation its card.
	CustomerID           string `json:"customerId"`
	CustomerEmailAddress string `json:"customerEmailAddress,omitempty"`
	// MerchantReference is Keera's payment id, so a transaction in
	// PostFinance's back office can be found in Keera.
	MerchantReference string `json:"merchantReference"`
	SuccessURL        string `json:"successUrl,omitempty"`
	FailedURL         string `json:"failedUrl,omitempty"`
	TokenizationMode  string `json:"tokenizationMode,omitempty"`
	// Token charges a saved card. CustomersPresence is then NOT_PRESENT.
	Token              int64             `json:"token,omitempty"`
	CustomersPresence  string            `json:"customersPresence,omitempty"`
	CompletionBehavior string            `json:"completionBehavior"`
	Language           string            `json:"language,omitempty"`
	MetaData           map[string]string `json:"metaData,omitempty"`
}

// Transaction is what Keera reads back of a transaction.
type Transaction struct {
	ID                  int64   `json:"id"`
	State               string  `json:"state"`
	Currency            string  `json:"currency"`
	AuthorizationAmount float64 `json:"authorizationAmount"`
	CustomerID          string  `json:"customerId"`
	MerchantReference   string  `json:"merchantReference"`
	UserFailureMessage  string  `json:"userFailureMessage"`
	Token               *struct {
		ID int64 `json:"id"`
	} `json:"token"`
}

// Paid reports whether the money is in. FULFILL is the state PostFinance
// says goods may be handed over in; COMPLETED can still be held back by a
// fraud check.
func (t Transaction) Paid() bool { return t.State == "FULFILL" }

// Failed reports whether the transaction will never be paid.
func (t Transaction) Failed() bool {
	return t.State == "FAILED" || t.State == "DECLINE" || t.State == "VOIDED"
}

// TokenID is the saved card the transaction made or used, or zero.
func (t Transaction) TokenID() int64 {
	if t.Token == nil {
		return 0
	}
	return t.Token.ID
}

// Card is how a saved card is shown: never its number.
type Card struct {
	// Label is what PostFinance calls the card, such as a brand and the last
	// digits.
	Label string
	// Expires is when PostFinance stops charging it. Zero when unknown.
	Expires time.Time
}

// Create creates a transaction.
func (c *Client) Create(ctx context.Context, t NewTransaction) (Transaction, error) {
	var out Transaction
	err := c.do(ctx, http.MethodPost, "/payment/transactions", t, &out)
	return out, err
}

// Get reads a transaction.
func (c *Client) Get(ctx context.Context, id int64) (Transaction, error) {
	var out Transaction
	err := c.do(ctx, http.MethodGet, "/payment/transactions/"+strconv.FormatInt(id, 10), nil, &out)
	return out, err
}

// PaymentPageURL is where to send the customer to pay a transaction.
func (c *Client) PaymentPageURL(ctx context.Context, id int64) (string, error) {
	var raw []byte
	err := c.do(ctx, http.MethodGet,
		"/payment/transactions/"+strconv.FormatInt(id, 10)+"/payment-page-url", nil, &raw)
	if err != nil {
		return "", err
	}
	u := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if !strings.HasPrefix(u, "https://") {
		return "", fmt.Errorf("postfinance: the payment page is not an https address: %q", u)
	}
	return u, nil
}

// Charge processes a transaction on a saved card, without the customer.
func (c *Client) Charge(ctx context.Context, id int64) (Transaction, error) {
	var out Transaction
	err := c.do(ctx, http.MethodPost,
		"/payment/transactions/"+strconv.FormatInt(id, 10)+"/process-without-interaction", nil, &out)
	return out, err
}

// Card reads how a saved card is shown.
func (c *Client) Card(ctx context.Context, token int64) (Card, error) {
	var v struct {
		Name      string `json:"name"`
		ExpiresOn string `json:"expiresOn"`
		Brand     *struct {
			Name map[string]string `json:"name"`
		} `json:"paymentMethodBrand"`
	}
	err := c.do(ctx, http.MethodGet,
		"/payment/tokens/"+strconv.FormatInt(token, 10)+"/active-version", nil, &v)
	if err != nil {
		return Card{}, err
	}
	card := Card{Label: v.Name}
	if v.Brand != nil {
		if brand := localized(v.Brand.Name); brand != "" && !strings.Contains(card.Label, brand) {
			card.Label = strings.TrimSpace(brand + " " + card.Label)
		}
	}
	if t, err := time.Parse(time.RFC3339, v.ExpiresOn); err == nil {
		card.Expires = t
	}
	return card, nil
}

// DeleteToken forgets a saved card at PostFinance.
func (c *Client) DeleteToken(ctx context.Context, token int64) error {
	return c.do(ctx, http.MethodDelete, "/payment/tokens/"+strconv.FormatInt(token, 10), nil, nil)
}

// localized picks English from a name PostFinance gives in several
// languages, or any of them.
func localized(names map[string]string) string {
	for _, lang := range []string{"en-US", "en-GB", "de-CH"} {
		if n := names[lang]; n != "" {
			return n
		}
	}
	for _, n := range names {
		return n
	}
	return ""
}

// Error is a refusal from PostFinance.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("postfinance: %d", e.Status)
	if e.Code != "" {
		msg += " " + e.Code
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

// maxResponse bounds what is read of an answer. A transaction is a few
// kilobytes.
const maxResponse = 1 << 20

// do sends one request. out is a pointer to decode JSON into, a *[]byte for
// a plain text answer, or nil.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	path = "/api/v2.0" + path
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.s.URL+path, body)
	if err != nil {
		return err
	}
	token, err := c.jwt(method, path, time.Now())
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Space", strconv.FormatInt(c.s.SpaceID, 10))
	req.Header.Set("Accept", "application/json, text/plain")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("postfinance: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("postfinance: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		var v struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &v)
		return &Error{Status: resp.StatusCode, Code: v.Code, Message: v.Message}
	}
	switch o := out.(type) {
	case nil:
		return nil
	case *[]byte:
		*o = raw
		return nil
	default:
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("postfinance: reading the answer: %w", err)
		}
		return nil
	}
}

// jwt signs one request, as PostFinance asks: HS256 over the method and the
// path with its query, issued now, so a captured token is useless for any
// other request and soon for this one too.
func (c *Client) jwt(method, path string, now time.Time) (string, error) {
	if len(c.s.AuthKey) == 0 {
		return "", errors.New("postfinance: no authentication key")
	}
	header, _ := json.Marshal(map[string]any{"alg": "HS256", "typ": "JWT", "ver": 1})
	payload, _ := json.Marshal(map[string]any{
		"sub":           c.s.UserID,
		"iat":           now.Unix(),
		"requestPath":   path,
		"requestMethod": method,
	})
	enc := base64.RawURLEncoding
	signed := enc.EncodeToString(header) + "." + enc.EncodeToString(payload)
	mac := hmac.New(sha256.New, c.s.AuthKey)
	mac.Write([]byte(signed))
	return signed + "." + enc.EncodeToString(mac.Sum(nil)), nil
}

// Amount writes micro-francs as PostFinance takes an amount, such as
// "108.10". Anything below a centime is dropped: Keera only asks for whole
// centimes.
func Amount(micros int64) json.Number {
	sign := ""
	if micros < 0 {
		sign, micros = "-", -micros
	}
	return json.Number(fmt.Sprintf("%s%d.%02d", sign, micros/1_000_000, micros%1_000_000/10_000))
}

// Percent writes basis points as a rate, such as "8.1".
func Percent(bp int64) json.Number {
	s := strconv.FormatFloat(float64(bp)/100, 'f', -1, 64)
	return json.Number(s)
}
