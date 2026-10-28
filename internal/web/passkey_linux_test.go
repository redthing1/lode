//go:build linux

package web

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/redthing1/lode/internal/instance"
)

type passkeyTestAuthenticator struct {
	id  []byte
	key *ecdsa.PrivateKey
}

func newPasskeyTestAuthenticator(t *testing.T) passkeyTestAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return passkeyTestAuthenticator{id: []byte("http-passkey-credential"), key: key}
}

func (a passkeyTestAuthenticator) registration(t *testing.T, challenge, origin string) []byte {
	t.Helper()
	pub := a.key.PublicKey
	coseKey, err := webauthncbor.Marshal(map[int64]any{
		1: int64(webauthncose.EllipticKey), 3: int64(webauthncose.AlgES256), -1: int64(webauthncose.P256),
		-2: pub.X.FillBytes(make([]byte, 32)), -3: pub.Y.FillBytes(make([]byte, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(origin)
	rpHash := sha256.Sum256([]byte(u.Hostname()))
	authData := append([]byte(nil), rpHash[:]...)
	authData = append(authData, byte(protocol.FlagUserPresent|protocol.FlagUserVerified|protocol.FlagAttestedCredentialData))
	authData = binary.BigEndian.AppendUint32(authData, 0)
	authData = append(authData, make([]byte, 16)...)
	authData = binary.BigEndian.AppendUint16(authData, uint16(len(a.id)))
	authData = append(authData, a.id...)
	authData = append(authData, coseKey...)
	attestation, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": authData})
	if err != nil {
		t.Fatal(err)
	}
	clientData, err := json.Marshal(map[string]any{"type": "webauthn.create", "challenge": challenge, "origin": origin, "crossOrigin": false})
	if err != nil {
		t.Fatal(err)
	}
	id := base64.RawURLEncoding.EncodeToString(a.id)
	body, err := json.Marshal(map[string]any{"id": id, "rawId": id, "type": "public-key", "response": map[string]any{
		"attestationObject": base64.RawURLEncoding.EncodeToString(attestation),
		"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientData),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func (a passkeyTestAuthenticator) assertion(t *testing.T, userID, challenge, origin string, counter uint32) []byte {
	return a.assertionWithUV(t, userID, challenge, origin, counter, true)
}

func (a passkeyTestAuthenticator) assertionWithUV(t *testing.T, userID, challenge, origin string, counter uint32, uv bool) []byte {
	t.Helper()
	u, _ := url.Parse(origin)
	rpHash := sha256.Sum256([]byte(u.Hostname()))
	authData := append([]byte(nil), rpHash[:]...)
	flags := protocol.FlagUserPresent
	if uv {
		flags |= protocol.FlagUserVerified
	}
	authData = append(authData, byte(flags))
	authData = binary.BigEndian.AppendUint32(authData, counter)
	clientData, err := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": challenge, "origin": origin, "crossOrigin": false})
	if err != nil {
		t.Fatal(err)
	}
	clientHash := sha256.Sum256(clientData)
	signed := append(append([]byte(nil), authData...), clientHash[:]...)
	digest := sha256.Sum256(signed)
	signature, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	id := base64.RawURLEncoding.EncodeToString(a.id)
	body, err := json.Marshal(map[string]any{"id": id, "rawId": id, "type": "public-key", "response": map[string]any{
		"authenticatorData": base64.RawURLEncoding.EncodeToString(authData),
		"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientData),
		"signature":         base64.RawURLEncoding.EncodeToString(signature),
		"userHandle":        base64.RawURLEncoding.EncodeToString([]byte(userID)),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

type passkeyChallenge struct {
	ID      string `json:"id"`
	Options struct {
		PublicKey struct {
			Challenge        string `json:"challenge"`
			UserVerification string `json:"userVerification"`
			AllowCredentials []any  `json:"allowCredentials"`
		} `json:"publicKey"`
	} `json:"options"`
}

type passkeyHTTPFixture struct {
	instance      *instance.Instance
	origin        string
	user          instance.User
	authenticator passkeyTestAuthenticator
}

func newPasskeyHTTPFixture(t *testing.T) passkeyHTTPFixture {
	t.Helper()
	inst, origin, setupCode := testServer(t)
	ctx := context.Background()
	if _, err := inst.Setup(ctx, setupCode, "passkey_admin", "a sufficiently long passphrase"); err != nil {
		t.Fatal(err)
	}
	user, err := inst.Store.CreateUser(ctx, "passkey_member", "temporary passphrase", false)
	if err != nil {
		t.Fatal(err)
	}
	user, err = inst.Store.SetPassword(ctx, user.ID, "a sufficiently long passphrase")
	if err != nil {
		t.Fatal(err)
	}
	security, err := instance.NewSecurity(inst.Store, origin)
	if err != nil {
		t.Fatal(err)
	}
	authenticator := newPasskeyTestAuthenticator(t)
	id, options, err := security.Begin(ctx, user.ID, "enrollment-session", "register")
	if err != nil {
		t.Fatal(err)
	}
	creation := options.(*protocol.CredentialCreation)
	request := httptest.NewRequest(http.MethodPost, origin+"/security/register/finish", bytes.NewReader(authenticator.registration(t, creation.Response.Challenge.String(), origin)))
	if _, err := security.FinishRegistration(ctx, user.ID, "enrollment-session", id, "HTTP test key", request); err != nil {
		t.Fatal(err)
	}
	return passkeyHTTPFixture{instance: inst, origin: origin, user: user, authenticator: authenticator}
}

func (f passkeyHTTPFixture) begin(t *testing.T, b *browser) passkeyChallenge {
	return f.beginAs(t, b, "")
}

func (f passkeyHTTPFixture) beginAs(t *testing.T, b *browser, username string) passkeyChallenge {
	t.Helper()
	body, err := json.Marshal(map[string]string{"username": username})
	if err != nil {
		t.Fatal(err)
	}
	response := b.request("/passkey/begin", http.MethodPost, body, "application/json", f.origin, "")
	wantStatus(t, response, http.StatusOK)
	var challenge passkeyChallenge
	if err := json.Unmarshal([]byte(response.body), &challenge); err != nil {
		t.Fatalf("decode begin response: %v (%s)", err, response.body)
	}
	if challenge.ID == "" || challenge.Options.PublicKey.Challenge == "" {
		t.Fatalf("incomplete passkey challenge: %+v", challenge)
	}
	return challenge
}

func TestPasskeyHTTPUsernameFirstUsesUserBoundUVChallenge(t *testing.T) {
	f := newPasskeyHTTPFixture(t)
	b := newBrowser(t, f.origin)
	challenge := f.beginAs(t, b, " PASSKEY_MEMBER ")
	if challenge.Options.PublicKey.UserVerification != "required" || len(challenge.Options.PublicKey.AllowCredentials) != 10 {
		t.Fatalf("username-first options did not allow this user's key with required UV: %+v", challenge.Options.PublicKey)
	}
	response := f.finish(t, b, challenge, 1)
	wantStatus(t, response, http.StatusOK)
	wantStatus(t, b.get("/settings/security"), http.StatusOK)
}

func TestPasskeyHTTPUsernameBeginUsesGenericOptionsForUnknownAccount(t *testing.T) {
	f := newPasskeyHTTPFixture(t)
	b := newBrowser(t, f.origin)
	known := f.beginAs(t, b, "passkey_member")
	unknown := f.beginAs(t, b, "not-a-real-account")
	if known.Options.PublicKey.UserVerification != "required" || unknown.Options.PublicKey.UserVerification != "required" || len(known.Options.PublicKey.AllowCredentials) != 10 || len(unknown.Options.PublicKey.AllowCredentials) != 10 {
		t.Fatalf("username begin responses have different shapes: known=%+v unknown=%+v", known.Options.PublicKey, unknown.Options.PublicKey)
	}
	var count int
	if err := f.instance.DB().QueryRow("SELECT count(*) FROM ceremonies WHERE id=?", unknown.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unusable dummy challenge has %d ceremony rows, error %v", count, err)
	}
	if response := f.finish(t, b, known, 1); response.status != http.StatusUnauthorized {
		t.Fatalf("new username begin did not replace prior challenge: %d", response.status)
	}
	if response := f.finish(t, b, unknown, 1); response.status != http.StatusUnauthorized {
		t.Fatalf("dummy username ceremony status %d, want 401", response.status)
	}
	if replay := f.finish(t, b, unknown, 2); replay.status != http.StatusUnauthorized {
		t.Fatalf("dummy ceremony replay status %d, want 401", replay.status)
	}
}

func TestPasskeyHTTPUsernameBeginReplacesDiscoverableChallenge(t *testing.T) {
	f := newPasskeyHTTPFixture(t)
	b := newBrowser(t, f.origin)
	discoverable := f.begin(t, b)
	username := f.beginAs(t, b, "passkey_member")
	if stale := f.finish(t, b, discoverable, 1); stale.status != http.StatusUnauthorized {
		t.Fatalf("username begin left anonymous challenge live: %d", stale.status)
	}
	if current := f.finish(t, b, username, 1); current.status != http.StatusOK {
		t.Fatalf("username passkey sign-in failed after replacing anonymous challenge: %d %s", current.status, current.body)
	}
}

func (f passkeyHTTPFixture) finish(t *testing.T, b *browser, challenge passkeyChallenge, count uint32) browserResponse {
	t.Helper()
	body := f.authenticator.assertion(t, f.user.ID, challenge.Options.PublicKey.Challenge, f.origin, count)
	return b.request("/passkey/finish?id="+url.QueryEscape(challenge.ID), http.MethodPost, body, "application/json", f.origin, "")
}

func TestPasskeyHTTPLoginIsPrimaryAndCookieIsNotAPIAuth(t *testing.T) {
	f := newPasskeyHTTPFixture(t)
	b := newBrowser(t, f.origin)
	challenge := f.begin(t, b)
	if challenge.Options.PublicKey.UserVerification != "required" || len(challenge.Options.PublicKey.AllowCredentials) != 0 {
		t.Fatalf("server did not request user-verified discoverable sign-in: %+v", challenge.Options.PublicKey)
	}
	response := f.finish(t, b, challenge, 1)
	wantStatus(t, response, http.StatusOK)
	if response.body != `{"next":"/"}`+"\n" {
		t.Fatalf("passkey finish response %q", response.body)
	}
	wantStatus(t, b.get("/settings/security"), http.StatusOK)
	if response := b.get("/verify"); response.status != http.StatusSeeOther || response.header.Get("Location") != "/" {
		t.Fatalf("passkey session entered pending verification: %d %q", response.status, response.header.Get("Location"))
	}
	api := b.get("/api/me")
	wantStatus(t, api, http.StatusUnauthorized)
	projectAPI := b.get("/p/no-project/v1/head?branch=main")
	wantStatus(t, projectAPI, http.StatusUnauthorized)
	if replay := f.finish(t, b, challenge, 2); replay.status != http.StatusUnauthorized {
		t.Fatalf("replayed assertion status %d, want 401: %s", replay.status, replay.body)
	}
}

func TestPasskeyHTTPRejectsMalformedAndCrossBrowserCeremonies(t *testing.T) {
	f := newPasskeyHTTPFixture(t)
	a := newBrowser(t, f.origin)
	b := newBrowser(t, f.origin)
	crossBrowser := f.begin(t, a)
	if response := f.finish(t, b, crossBrowser, 1); response.status != http.StatusUnauthorized {
		t.Fatalf("cross-browser challenge status %d, want 401", response.status)
	}
	if response := f.finish(t, a, crossBrowser, 1); response.status != http.StatusOK {
		t.Fatalf("challenge did not remain bound to its initiating browser: %d %s", response.status, response.body)
	}
	malformed := f.begin(t, b)
	bad := b.request("/passkey/finish?id="+url.QueryEscape(malformed.ID), http.MethodPost, []byte("{"), "application/json", f.origin, "")
	if bad.status != http.StatusUnauthorized {
		t.Fatalf("malformed assertion status %d, want 401", bad.status)
	}
	if replay := f.finish(t, b, malformed, 2); replay.status != http.StatusUnauthorized {
		t.Fatalf("malformed challenge could be retried: status %d", replay.status)
	}
}

func TestPasskeyHTTPRejectsAssertionWithoutUserVerification(t *testing.T) {
	f := newPasskeyHTTPFixture(t)
	b := newBrowser(t, f.origin)
	challenge := f.begin(t, b)
	body := f.authenticator.assertionWithUV(t, f.user.ID, challenge.Options.PublicKey.Challenge, f.origin, 1, false)
	response := b.request("/passkey/finish?id="+url.QueryEscape(challenge.ID), http.MethodPost, body, "application/json", f.origin, "")
	if response.status != http.StatusUnauthorized {
		t.Fatalf("assertion without UV status %d, want 401", response.status)
	}
	if replay := f.finish(t, b, challenge, 1); replay.status != http.StatusUnauthorized {
		t.Fatalf("non-UV challenge could be retried: status %d", replay.status)
	}
}

func TestPasskeyHTTPRejectsMismatchedUserHandle(t *testing.T) {
	f := newPasskeyHTTPFixture(t)
	b := newBrowser(t, f.origin)
	challenge := f.begin(t, b)
	body := f.authenticator.assertion(t, "another-account-id", challenge.Options.PublicKey.Challenge, f.origin, 1)
	response := b.request("/passkey/finish?id="+url.QueryEscape(challenge.ID), http.MethodPost, body, "application/json", f.origin, "")
	if response.status != http.StatusUnauthorized {
		t.Fatalf("mismatched WebAuthn user handle status %d, want 401", response.status)
	}
}

func TestPasskeyHTTPRejectsDisabledAndForcedPasswordChange(t *testing.T) {
	f := newPasskeyHTTPFixture(t)
	b := newBrowser(t, f.origin)
	disabledChallenge := f.begin(t, b)
	if _, err := f.instance.Store.SetUserState(context.Background(), f.user.ID, true, false); err != nil {
		t.Fatal(err)
	}
	if response := f.finish(t, b, disabledChallenge, 1); response.status != http.StatusUnauthorized {
		t.Fatalf("disabled account passkey sign-in status %d, want 401", response.status)
	}
	if _, err := f.instance.Store.SetUserState(context.Background(), f.user.ID, false, false); err != nil {
		t.Fatal(err)
	}
	forcedChallenge := f.begin(t, b)
	if _, err := f.instance.DB().ExecContext(context.Background(), "UPDATE users SET force_password_change=1,auth_version=auth_version+1 WHERE id=?", f.user.ID); err != nil {
		t.Fatal(err)
	}
	if response := f.finish(t, b, forcedChallenge, 2); response.status != http.StatusUnauthorized {
		t.Fatalf("forced-password account passkey sign-in status %d, want 401", response.status)
	}
}

func TestPasskeyHTTPAnonymousChallengeResolvesCurrentAuthVersion(t *testing.T) {
	f := newPasskeyHTTPFixture(t)
	b := newBrowser(t, f.origin)
	challenge := f.begin(t, b)
	if _, err := f.instance.DB().ExecContext(context.Background(), "UPDATE users SET auth_version=auth_version+1 WHERE id=?", f.user.ID); err != nil {
		t.Fatal(err)
	}
	// An anonymous challenge cannot be pre-bound to one account version because
	// the discoverable credential identifies the account only after assertion.
	// The assertion still succeeds when the account remains enabled and usable;
	// the security service's final CAS protects changes during verification.
	response := f.finish(t, b, challenge, 1)
	if response.status != http.StatusOK {
		t.Fatalf("usable account after auth-version update was rejected: %d %s", response.status, response.body)
	}
}

func TestPasskeyHTTPUsernameFirstRejectsAuthVersionChange(t *testing.T) {
	f := newPasskeyHTTPFixture(t)
	b := newBrowser(t, f.origin)
	challenge := f.beginAs(t, b, "passkey_member")
	if _, err := f.instance.DB().ExecContext(context.Background(), "UPDATE users SET auth_version=auth_version+1 WHERE id=?", f.user.ID); err != nil {
		t.Fatal(err)
	}
	if response := f.finish(t, b, challenge, 1); response.status != http.StatusUnauthorized {
		t.Fatalf("username-bound stale ceremony status %d, want 401", response.status)
	}
}
