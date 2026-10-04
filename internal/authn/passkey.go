package authn

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// Passkeys are WebAuthn credentials that Keera checks itself, for accounts no
// directory vouches for.
//
// Only what passkeys need is implemented. Attestation is "none", so the
// gateway never parses CBOR: the browser hands over the public key as
// SubjectPublicKeyInfo (getPublicKey), which Go reads directly. Without
// attestation nothing at registration is signed anyway, so this trusts the
// browser exactly as much as parsing the attestation object would.

// PasskeyProvider is the name a passkey sign-in goes by wherever a provider is
// named: in /auth/login?provider=, in `keera login --provider`, in the audit
// log. No identity provider may use it.
const PasskeyProvider = "passkey"

// The COSE algorithms a passkey may use, in the order the browser is asked to
// prefer them. RS256 is for older Windows Hello.
const (
	AlgES256 = -7
	AlgEdDSA = -8
	AlgRS256 = -257
)

const (
	// PasskeyTimeout is how long the browser waits for the person to touch
	// their authenticator.
	PasskeyTimeout = 5 * time.Minute
	// PasskeyChallengeTTL bounds a registration in flight. It is a little
	// longer than the browser's own timeout.
	PasskeyChallengeTTL = PasskeyTimeout + time.Minute
	// PasskeyLinkTTL is how long a set-up link works. It is handed over by
	// hand, so it lasts a working day and a bit.
	PasskeyLinkTTL = 24 * time.Hour
)

// The authenticator data flags Keera checks.
const (
	flagUserPresent  = 0x01
	flagUserVerified = 0x04
	flagAttested     = 0x40
)

// PasskeyExternalID marks an account that signs in with passkeys. The
// provider part is reserved, so no directory can issue it, and a first SSO
// sign-in never adopts the row: it already has a subject.
func PasskeyExternalID(userID string) string {
	return ReservedProvider + ":" + PasskeyProvider + ":" + userID
}

// IsPasskeyAccount reports whether an external ID is one PasskeyExternalID
// made.
func IsPasskeyAccount(externalID string) bool {
	return strings.HasPrefix(externalID, ReservedProvider+":"+PasskeyProvider+":")
}

// RelyingParty is this gateway as WebAuthn sees it.
type RelyingParty struct {
	id     string
	origin string
}

// NewRelyingParty derives the relying party from the public URL. The RP ID is
// the host name, so a passkey works only on that name: moving the gateway to
// another domain makes every passkey useless.
func NewRelyingParty(publicURL string) (*RelyingParty, error) {
	u, err := url.Parse(publicURL)
	if err != nil || u.Host == "" {
		return nil, errors.New("passkeys need KEERA_PUBLIC_URL, the address a browser " +
			"reaches the panel on")
	}
	host := u.Hostname()
	// Browsers allow WebAuthn only in a secure context, and never on an IP
	// address.
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && (host == "localhost" || strings.HasSuffix(host, ".localhost")):
	default:
		return nil, fmt.Errorf("passkeys need KEERA_PUBLIC_URL to be https, or http on "+
			"localhost; %s is neither", publicURL)
	}
	if net.ParseIP(host) != nil {
		return nil, fmt.Errorf("passkeys need a host name in KEERA_PUBLIC_URL, not an "+
			"address; browsers refuse %s", host)
	}
	return &RelyingParty{id: host, origin: u.Scheme + "://" + u.Host}, nil
}

// ID is the RP ID.
func (rp *RelyingParty) ID() string { return rp.id }

// Origin is the only origin a ceremony may come from.
func (rp *RelyingParty) Origin() string { return rp.origin }

// NewPasskeyChallenge mints a ceremony's challenge, as base64url.
func NewPasskeyChallenge() string { return randomToken() }

// NewPasskeyLink mints the token of a set-up link and the hash stored for it.
func NewPasskeyLink() (token string, hash []byte) {
	token = randomToken()
	return token, HashPasskeyLink(token)
}

// HashPasskeyLink returns the value stored in passkey_links.id for token.
func HashPasskeyLink(token string) []byte { return sum256(token) }

// credentialParam and credentialRef are the JSON forms of
// PublicKeyCredentialParameters and PublicKeyCredentialDescriptor. Binary
// values are base64url, which the panel decodes.
type credentialParam struct {
	Type string `json:"type"`
	Alg  int    `json:"alg"`
}

type credentialRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// CreationOptions is what navigator.credentials.create is called with.
type CreationOptions struct {
	Challenge string `json:"challenge"`
	RP        struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"rp"`
	User struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	} `json:"user"`
	PubKeyCredParams       []credentialParam `json:"pubKeyCredParams"`
	Timeout                int64             `json:"timeout"`
	ExcludeCredentials     []credentialRef   `json:"excludeCredentials"`
	AuthenticatorSelection struct {
		ResidentKey        string `json:"residentKey"`
		RequireResidentKey bool   `json:"requireResidentKey"`
		UserVerification   string `json:"userVerification"`
	} `json:"authenticatorSelection"`
	Attestation string `json:"attestation"`
}

// CreationOptions asks the browser for a discoverable credential with user
// verification, so the passkey alone signs the person in. exclude lists the
// credentials the person already has, so one authenticator is not registered
// twice.
func (rp *RelyingParty) CreationOptions(challenge, userID, email string,
	exclude [][]byte) CreationOptions {
	var o CreationOptions
	o.Challenge = challenge
	o.RP.ID = rp.id
	o.RP.Name = "Keera"
	// The user handle is the user's id, which says nothing about the person.
	o.User.ID = b64(UserHandle(userID))
	o.User.Name = email
	o.User.DisplayName = email
	o.PubKeyCredParams = []credentialParam{
		{"public-key", AlgES256}, {"public-key", AlgEdDSA}, {"public-key", AlgRS256},
	}
	o.Timeout = PasskeyTimeout.Milliseconds()
	o.ExcludeCredentials = []credentialRef{}
	for _, id := range exclude {
		o.ExcludeCredentials = append(o.ExcludeCredentials, credentialRef{"public-key", b64(id)})
	}
	o.AuthenticatorSelection.ResidentKey = "required"
	o.AuthenticatorSelection.RequireResidentKey = true
	o.AuthenticatorSelection.UserVerification = "required"
	o.Attestation = "none"
	return o
}

// RequestOptions is what navigator.credentials.get is called with.
type RequestOptions struct {
	Challenge        string          `json:"challenge"`
	RPID             string          `json:"rpId"`
	Timeout          int64           `json:"timeout"`
	UserVerification string          `json:"userVerification"`
	AllowCredentials []credentialRef `json:"allowCredentials"`
}

// RequestOptions lists no credentials, so the browser offers every passkey it
// has for this gateway and the person picks one.
func (rp *RelyingParty) RequestOptions(challenge string) RequestOptions {
	return RequestOptions{
		Challenge:        challenge,
		RPID:             rp.id,
		Timeout:          PasskeyTimeout.Milliseconds(),
		UserVerification: "required",
		AllowCredentials: []credentialRef{},
	}
}

// UserHandle is the WebAuthn user handle for a user id.
func UserHandle(userID string) []byte { return []byte(userID) }

// Registration is what the browser returns from navigator.credentials.create.
// Every binary field is base64url.
type Registration struct {
	ID                string `json:"id"`
	ClientDataJSON    string `json:"client_data_json"`
	AuthenticatorData string `json:"authenticator_data"`
	PublicKey         string `json:"public_key"`
	Algorithm         int    `json:"algorithm"`
}

// Credential is a registered passkey, as stored.
type Credential struct {
	ID        []byte
	PublicKey []byte // SubjectPublicKeyInfo, DER
	Algorithm int
	SignCount uint32
}

// ErrPasskey is the cause of every refused ceremony. The detail is for the
// log; a person is told the ceremony failed.
var ErrPasskey = errors.New("authn: passkey refused")

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrPasskey}, args...)...)
}

// VerifyRegistration checks a new passkey against the challenge it was
// created for, and returns it ready to store.
func (rp *RelyingParty) VerifyRegistration(challenge string, r Registration) (Credential, error) {
	rawID, err := unb64(r.ID)
	if err != nil || len(rawID) == 0 || len(rawID) > 1023 {
		return Credential{}, refuse("the credential id is not usable")
	}
	if err := rp.checkClientData(r.ClientDataJSON, "webauthn.create", challenge); err != nil {
		return Credential{}, err
	}
	auth, err := rp.parseAuthData(r.AuthenticatorData)
	if err != nil {
		return Credential{}, err
	}
	if auth.flags&flagAttested == 0 {
		return Credential{}, refuse("the authenticator data has no credential in it")
	}
	// After the 37-byte header: a 16-byte AAGUID, a 2-byte length, the id.
	rest := auth.raw[37:]
	if len(rest) < 18 {
		return Credential{}, refuse("the attested credential data is cut short")
	}
	n := int(binary.BigEndian.Uint16(rest[16:18]))
	if len(rest) < 18+n || !bytes.Equal(rest[18:18+n], rawID) {
		return Credential{}, refuse("the credential id does not match the authenticator data")
	}

	spki, err := unb64(r.PublicKey)
	if err != nil || len(spki) == 0 {
		return Credential{}, refuse("the browser did not hand over the public key; " +
			"it may be too old for passkeys")
	}
	if _, err := parseKey(spki, r.Algorithm); err != nil {
		return Credential{}, err
	}
	return Credential{
		ID: rawID, PublicKey: spki, Algorithm: r.Algorithm, SignCount: auth.signCount,
	}, nil
}

// Assertion is what the browser returns from navigator.credentials.get. Every
// binary field is base64url.
type Assertion struct {
	ID                string `json:"id"`
	ClientDataJSON    string `json:"client_data_json"`
	AuthenticatorData string `json:"authenticator_data"`
	Signature         string `json:"signature"`
	UserHandle        string `json:"user_handle"`
}

// CredentialID decodes the id of the passkey an assertion names, to look it up.
func (a Assertion) CredentialID() ([]byte, error) {
	id, err := unb64(a.ID)
	if err != nil || len(id) == 0 {
		return nil, refuse("the credential id is not usable")
	}
	return id, nil
}

// OwnedBy reports whether the user handle names userID. A missing handle
// passes, since the credential id already picked the passkey.
func (a Assertion) OwnedBy(userID string) bool {
	if a.UserHandle == "" {
		return true
	}
	got, err := unb64(a.UserHandle)
	return err == nil && subtle.ConstantTimeCompare(got, UserHandle(userID)) == 1
}

// VerifyAssertion checks a sign-in against the challenge it answers and the
// stored passkey. It returns the new signature counter to store.
//
// The caller has looked the credential up by id. It also checks that the user
// handle, if any, is that credential's owner.
func (rp *RelyingParty) VerifyAssertion(challenge string, a Assertion, cred Credential) (uint32, error) {
	if err := rp.checkClientData(a.ClientDataJSON, "webauthn.get", challenge); err != nil {
		return 0, err
	}
	auth, err := rp.parseAuthData(a.AuthenticatorData)
	if err != nil {
		return 0, err
	}
	clientData, _ := unb64(a.ClientDataJSON)
	sig, err := unb64(a.Signature)
	if err != nil || len(sig) == 0 {
		return 0, refuse("the signature is not usable")
	}
	key, err := parseKey(cred.PublicKey, cred.Algorithm)
	if err != nil {
		return 0, err
	}
	hash := sha256.Sum256(clientData)
	signed := append(append([]byte{}, auth.raw...), hash[:]...)
	if !verify(key, cred.Algorithm, signed, sig) {
		return 0, refuse("the signature does not match the passkey")
	}
	// A counter that does not move forward means a copy of the key signed in.
	// Synced passkeys always send zero, which is fine.
	if (auth.signCount != 0 || cred.SignCount != 0) && auth.signCount <= cred.SignCount {
		return 0, refuse("the signature counter went backwards (%d after %d); the passkey "+
			"may have been copied", auth.signCount, cred.SignCount)
	}
	return auth.signCount, nil
}

// checkClientData checks what the browser says the ceremony was: its kind, its
// challenge, and the page that ran it. The origin check is what makes a
// passkey useless to a phishing site.
func (rp *RelyingParty) checkClientData(encoded, kind, challenge string) error {
	raw, err := unb64(encoded)
	if err != nil {
		return refuse("the client data is not base64url")
	}
	var cd struct {
		Type        string `json:"type"`
		Challenge   string `json:"challenge"`
		Origin      string `json:"origin"`
		CrossOrigin bool   `json:"crossOrigin"`
	}
	if err := json.Unmarshal(raw, &cd); err != nil {
		return refuse("the client data is not JSON")
	}
	switch {
	case cd.Type != kind:
		return refuse("the ceremony is %q, want %q", cd.Type, kind)
	case challenge == "" ||
		subtle.ConstantTimeCompare([]byte(cd.Challenge), []byte(challenge)) != 1:
		return refuse("the challenge does not match")
	case cd.Origin != rp.origin:
		return refuse("the ceremony ran on %s, want %s", cd.Origin, rp.origin)
	case cd.CrossOrigin:
		return refuse("the ceremony ran in a frame from another site")
	}
	return nil
}

type authData struct {
	raw       []byte
	flags     byte
	signCount uint32
}

// parseAuthData reads the fixed header of the authenticator data: the RP ID
// hash, the flags and the counter. Both ceremonies require user verification,
// so the passkey stands in for a password and a second factor at once.
func (rp *RelyingParty) parseAuthData(encoded string) (authData, error) {
	raw, err := unb64(encoded)
	if err != nil || len(raw) < 37 {
		return authData{}, refuse("the authenticator data is cut short")
	}
	want := sha256.Sum256([]byte(rp.id))
	if subtle.ConstantTimeCompare(raw[:32], want[:]) != 1 {
		return authData{}, refuse("the passkey is for another site")
	}
	a := authData{raw: raw, flags: raw[32], signCount: binary.BigEndian.Uint32(raw[33:37])}
	if a.flags&flagUserPresent == 0 {
		return authData{}, refuse("the authenticator did not check that someone was there")
	}
	if a.flags&flagUserVerified == 0 {
		return authData{}, refuse("the authenticator did not verify the person " +
			"with a PIN or biometrics")
	}
	return a, nil
}

// parseKey reads a stored public key and checks it is what the algorithm
// says.
func parseKey(spki []byte, alg int) (crypto.PublicKey, error) {
	key, err := x509.ParsePKIXPublicKey(spki)
	if err != nil {
		return nil, refuse("the public key is not readable: %v", err)
	}
	switch k := key.(type) {
	case *ecdsa.PublicKey:
		if alg == AlgES256 && k.Curve == elliptic.P256() {
			return k, nil
		}
	case ed25519.PublicKey:
		if alg == AlgEdDSA {
			return k, nil
		}
	case *rsa.PublicKey:
		if alg == AlgRS256 && k.N.BitLen() >= 2048 {
			return k, nil
		}
	}
	return nil, refuse("a %T does not go with algorithm %d", key, alg)
}

func verify(key crypto.PublicKey, alg int, signed, sig []byte) bool {
	switch alg {
	case AlgES256:
		sum := sha256.Sum256(signed)
		return ecdsa.VerifyASN1(key.(*ecdsa.PublicKey), sum[:], sig)
	case AlgEdDSA:
		return ed25519.Verify(key.(ed25519.PublicKey), signed, sig)
	case AlgRS256:
		sum := sha256.Sum256(signed)
		return rsa.VerifyPKCS1v15(key.(*rsa.PublicKey), crypto.SHA256, sum[:], sig) == nil
	}
	return false
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// unb64 accepts base64url with or without padding, since browsers differ.
func unb64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}
