package cli

import (
	"net/http"
	"testing"
)

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
