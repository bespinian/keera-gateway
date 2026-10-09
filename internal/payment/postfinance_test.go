package payment

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var testKey = []byte("an application user's key")

// checkJWT verifies a request's token as PostFinance does, and returns what
// it claims.
func checkJWT(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		t.Fatalf("no bearer token: %q", r.Header.Get("Authorization"))
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	mac := hmac.New(sha256.New, testKey)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if got, _ := base64.RawURLEncoding.DecodeString(parts[2]); !hmac.Equal(got, mac.Sum(nil)) {
		t.Fatal("the signature does not verify")
	}
	var header, claims map[string]any
	raw, _ := base64.RawURLEncoding.DecodeString(parts[0])
	_ = json.Unmarshal(raw, &header)
	raw, _ = base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(raw, &claims)
	if header["alg"] != "HS256" || header["ver"] != float64(1) {
		t.Errorf("header = %v", header)
	}
	return claims
}

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(Settings{URL: srv.URL, SpaceID: 4711, UserID: "123456", AuthKey: testKey})
}

// Every request is signed for its own method and path, in the space it is for.
func TestARequestIsSignedForItsMethodAndPath(t *testing.T) {
	var body map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		claims := checkJWT(t, r)
		if claims["sub"] != "123456" || claims["requestMethod"] != "POST" ||
			claims["requestPath"] != "/api/v2.0/payment/transactions" {
			t.Errorf("claims = %v", claims)
		}
		if iat, _ := claims["iat"].(float64); time.Since(time.Unix(int64(iat), 0)) > time.Minute {
			t.Errorf("issued at %v", claims["iat"])
		}
		if r.Header.Get("Space") != "4711" {
			t.Errorf("space = %q", r.Header.Get("Space"))
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":42,"state":"PENDING","currency":"CHF"}`)
	})
	tx, err := c.Create(context.Background(), NewTransaction{
		Currency: "CHF", CustomerID: "org_1", MerchantReference: "pay_1",
		CompletionBehavior: "COMPLETE_IMMEDIATELY",
		LineItems: []LineItem{{Name: "Keera credit", Quantity: 1, Type: "PRODUCT",
			UniqueID: "credit", AmountIncludingTax: Amount(108_100_000),
			Taxes: []Tax{{Rate: Percent(810), Title: "VAT"}}}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if tx.ID != 42 || tx.State != "PENDING" {
		t.Errorf("transaction = %+v", tx)
	}
	item := body["lineItems"].([]any)[0].(map[string]any)
	tax := item["taxes"].([]any)[0].(map[string]any)
	if item["amountIncludingTax"] != 108.1 || tax["rate"] != 8.1 || body["customerId"] != "org_1" {
		t.Errorf("sent %v", body)
	}
}

func TestThePaymentPageIsReadAsText(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if claims := checkJWT(t, r); claims["requestPath"] != "/api/v2.0/payment/transactions/42/payment-page-url" {
			t.Errorf("path = %v", claims["requestPath"])
		}
		_, _ = io.WriteString(w, "https://checkout.postfinance.ch/s/4711/payment/transaction/pay/42")
	})
	u, err := c.PaymentPageURL(context.Background(), 42)
	if err != nil || u != "https://checkout.postfinance.ch/s/4711/payment/transaction/pay/42" {
		t.Errorf("url = %q, %v", u, err)
	}
}

func TestAPaymentPageThatIsNotHTTPSIsRefused(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "javascript:alert(1)")
	})
	if _, err := c.PaymentPageURL(context.Background(), 42); err == nil {
		t.Error("an address that is not https was handed out")
	}
}

func TestATransactionSaysWhetherItIsPaid(t *testing.T) {
	for state, want := range map[string][2]bool{
		"FULFILL": {true, false}, "COMPLETED": {false, false}, "PROCESSING": {false, false},
		"FAILED": {false, true}, "DECLINE": {false, true}, "VOIDED": {false, true},
	} {
		tx := Transaction{State: state}
		if tx.Paid() != want[0] || tx.Failed() != want[1] {
			t.Errorf("%s: paid %v failed %v, want %v", state, tx.Paid(), tx.Failed(), want)
		}
	}
}

func TestASavedCardIsShownByBrandAndLabel(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if claims := checkJWT(t, r); claims["requestPath"] != "/api/v2.0/payment/tokens/7/active-version" {
			t.Errorf("path = %v", claims["requestPath"])
		}
		_, _ = io.WriteString(w, `{"name":"•••• 4821","expiresOn":"2028-09-30T00:00:00Z",
			"paymentMethodBrand":{"name":{"de-CH":"Visa","en-US":"Visa"}}}`)
	})
	card, err := c.Card(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if card.Label != "Visa •••• 4821" || card.Expires.Year() != 2028 {
		t.Errorf("card = %+v", card)
	}
}

func TestARefusalCarriesPostFinancesReason(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"code":"invalid_currency","message":"EUR is not enabled"}`)
	})
	_, err := c.Get(context.Background(), 1)
	var e *Error
	if !errors.As(err, &e) || e.Status != 422 || e.Code != "invalid_currency" ||
		!strings.Contains(err.Error(), "EUR is not enabled") {
		t.Errorf("err = %v", err)
	}
}

func TestAmountsAreWholeCentimes(t *testing.T) {
	for micros, want := range map[int64]string{
		100_000_000: "100.00", 108_100_000: "108.10", 50_000: "0.05", 1_234_567: "1.23",
		-20_000_000: "-20.00",
	} {
		if got := Amount(micros); string(got) != want {
			t.Errorf("Amount(%d) = %s, want %s", micros, got, want)
		}
	}
	if got := Percent(810); got != "8.1" {
		t.Errorf("Percent(810) = %s", got)
	}
}
