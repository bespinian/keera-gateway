// Package id generates the short, sortable, prefixed identifiers used for
// every row Keera Gateway owns.
package id

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"strings"
	"time"
)

var enc = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

// New returns an identifier such as "org_06c1k2rt8g3m4n5p6q7r8s9t0v". The first
// six bytes are the time in milliseconds, so identifiers sort by creation time;
// the other ten are random.
func New(prefix string) string {
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(time.Now().UnixMilli()))

	var b [16]byte
	copy(b[:6], ts[2:]) // the low 48 bits hold every realistic timestamp
	if _, err := rand.Read(b[6:]); err != nil {
		panic("id: entropy source failed: " + err.Error())
	}
	return prefix + "_" + enc.EncodeToString(b[:])
}

// HasPrefix reports whether s looks like an identifier minted with prefix.
func HasPrefix(s, prefix string) bool {
	return strings.HasPrefix(s, prefix+"_") && len(s) == len(prefix)+1+26
}
