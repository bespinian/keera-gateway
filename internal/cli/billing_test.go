package cli

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/bespinian/keera-gateway/internal/store"
)

// A row Keera Agents billed names the item and its quantity, where a model's
// row has its tokens.
func TestPrintBillingShowsKeeraAgentsItems(t *testing.T) {
	plain(t)
	var buf bytes.Buffer
	w := newTable(&buf)
	printBilling(w, billingResponse{Month: "2026-11", Data: []store.BillingRow{{
		OrgID: "org_1", OrgName: "Example Bank", Service: store.ServiceAgents, Item: "CPU",
		Unit: "CPU-hours", QuantityMicros: 312_504_999, Calls: 720, BilledMicros: 15_630_000,
	}}, Total: billingTotal{BilledMicros: 15_630_000}})
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Keera Agents: CPU", "312.5 CPU-hours", "CHF 15.63"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("no %q in:\n%s", want, buf.String())
		}
	}
	// Its lines are hours, not calls.
	if strings.Contains(buf.String(), "720") {
		t.Errorf("the hours are shown as calls:\n%s", buf.String())
	}
}

func TestReceiptName(t *testing.T) {
	for header, want := range map[string]string{
		`attachment; filename=keera-receipt-2026-09-30-pay_1.pdf`: "keera-receipt-2026-09-30-pay_1.pdf",
		// A name that climbs out of the directory keeps only its base.
		`attachment; filename="../../etc/passwd"`: "passwd",
		`attachment; filename=".."`:               "",
		`attachment`:                              "",
		``:                                        "",
	} {
		h := http.Header{}
		h.Set("Content-Disposition", header)
		if got := receiptName(h); got != want {
			t.Errorf("receiptName(%q) = %q, want %q", header, got, want)
		}
	}
}
