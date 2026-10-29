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
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
)

const testRPID = "localhost"

// Synthetic responses exercise the real protocol verifier through HTTP.
type syntheticAuthenticator struct {
	id  []byte
	key *ecdsa.PrivateKey
}

func newSyntheticAuthenticator(t *testing.T, id string) syntheticAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return syntheticAuthenticator{id: []byte(id), key: key}
}

func registrationResponse(t *testing.T, auth syntheticAuthenticator, challenge, origin string) []byte {
	t.Helper()
	pub := auth.key.PublicKey
	coseKey, err := webauthncbor.Marshal(map[int64]any{
		1:  int64(webauthncose.EllipticKey),
		3:  int64(webauthncose.AlgES256),
		-1: int64(webauthncose.P256),
		-2: pub.X.FillBytes(make([]byte, 32)),
		-3: pub.Y.FillBytes(make([]byte, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	authData := make([]byte, 0, 37+16+2+len(auth.id)+len(coseKey))
	rpHash := sha256.Sum256([]byte(testRPID))
	authData = append(authData, rpHash[:]...)
	authData = append(authData, byte(protocol.FlagUserPresent|protocol.FlagUserVerified|protocol.FlagAttestedCredentialData))
	authData = binary.BigEndian.AppendUint32(authData, 0)
	authData = append(authData, make([]byte, 16)...)
	authData = binary.BigEndian.AppendUint16(authData, uint16(len(auth.id)))
	authData = append(authData, auth.id...)
	authData = append(authData, coseKey...)
	attestation, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": authData})
	if err != nil {
		t.Fatal(err)
	}
	clientData, err := json.Marshal(map[string]any{"type": "webauthn.create", "challenge": challenge, "origin": origin, "crossOrigin": false})
	if err != nil {
		t.Fatal(err)
	}
	id := base64.RawURLEncoding.EncodeToString(auth.id)
	body, err := json.Marshal(map[string]any{"id": id, "rawId": id, "type": "public-key", "response": map[string]any{"attestationObject": base64.RawURLEncoding.EncodeToString(attestation), "clientDataJSON": base64.RawURLEncoding.EncodeToString(clientData)}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func assertionResponse(t *testing.T, auth syntheticAuthenticator, challenge, origin string, counter uint32) []byte {
	t.Helper()
	return assertionWithFlags(t, auth, challenge, origin, counter, byte(protocol.FlagUserPresent|protocol.FlagUserVerified))
}

func assertionWithFlags(t *testing.T, auth syntheticAuthenticator, challenge, origin string, counter uint32, flags byte) []byte {
	t.Helper()
	authData := make([]byte, 0, 37)
	rpHash := sha256.Sum256([]byte(testRPID))
	authData = append(authData, rpHash[:]...)
	authData = append(authData, flags)
	authData = binary.BigEndian.AppendUint32(authData, counter)
	clientData, err := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": challenge, "origin": origin, "crossOrigin": false})
	if err != nil {
		t.Fatal(err)
	}
	clientHash := sha256.Sum256(clientData)
	signed := append(append([]byte(nil), authData...), clientHash[:]...)
	digest := sha256.Sum256(signed)
	signature, err := ecdsa.SignASN1(rand.Reader, auth.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	id := base64.RawURLEncoding.EncodeToString(auth.id)
	body, err := json.Marshal(map[string]any{"id": id, "rawId": id, "type": "public-key", "response": map[string]any{"authenticatorData": base64.RawURLEncoding.EncodeToString(authData), "clientDataJSON": base64.RawURLEncoding.EncodeToString(clientData), "signature": base64.RawURLEncoding.EncodeToString(signature)}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func passkeyAssertionResponse(t *testing.T, auth syntheticAuthenticator, userID, challenge, origin string, counter uint32) []byte {
	t.Helper()
	var response map[string]any
	if err := json.Unmarshal(assertionResponse(t, auth, challenge, origin, counter), &response); err != nil {
		t.Fatal(err)
	}
	credentialResponse := response["response"].(map[string]any)
	credentialResponse["userHandle"] = base64.RawURLEncoding.EncodeToString([]byte(userID))
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func (b *browserSession) keyJSON(path string, body []byte, want int) []byte {
	b.t.Helper()
	r, err := http.NewRequest("POST", b.origin+path, bytes.NewReader(body))
	if err != nil {
		b.t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", b.origin)
	response, err := b.client.Do(r)
	if err != nil {
		b.t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		b.t.Fatal(err)
	}
	if response.StatusCode != want {
		b.t.Fatalf("%s: status %d, expected %d: %s", path, response.StatusCode, want, data)
	}
	return data
}

type browserCeremony struct {
	ID      string
	Options struct {
		PublicKey struct {
			Challenge        string
			User             struct{ ID string }
			AllowCredentials []any
		}
	}
}

func beginBrowserKey(t *testing.T, b *browserSession, path string, form url.Values) browserCeremony {
	t.Helper()
	var c browserCeremony
	if err := json.Unmarshal([]byte(b.request("POST", path, form, 200)), &c); err != nil {
		t.Fatal(err)
	}
	if c.ID == "" || c.Options.PublicKey.Challenge == "" {
		t.Fatal("missing ceremony")
	}
	return c
}
func enrollBrowserKey(t *testing.T, b *browserSession, auth syntheticAuthenticator, mode string) []string {
	t.Helper()
	c := beginBrowserKey(t, b, "/settings/security/keys", url.Values{"current": {browserPassword}, "name": {"Workstation"}, "mode": {mode}})
	var reply keyReply
	data := b.keyJSON("/settings/security/keys/finish?id="+c.ID, registrationResponse(t, auth, c.Options.PublicKey.Challenge, b.origin), 200)
	if err := json.Unmarshal(data, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Next != "/settings/security" {
		t.Fatal("incorrect enrollment destination")
	}
	return reply.Codes
}
func loadedBrowserSession(t *testing.T, b *browserSession, sessions *scs.SessionManager) (context.Context, *http.Cookie) {
	t.Helper()
	origin, _ := url.Parse(b.origin)
	for _, cookie := range b.client.Jar.Cookies(origin) {
		if cookie.Name != sessions.Cookie.Name {
			continue
		}
		ctx, err := sessions.Load(context.Background(), cookie.Value)
		if err != nil {
			t.Fatal(err)
		}
		return ctx, cookie
	}
	t.Fatal("session missing")
	return nil, nil
}
func restoreBrowserCookie(b *browserSession, cookie *http.Cookie) {
	origin, _ := url.Parse(b.origin)
	b.client.Jar.SetCookies(origin, []*http.Cookie{cookie})
}

func TestBrowserLogoutRejectsLateSessionCommit(t *testing.T) {
	f := apiTest(t)
	b := newBrowserSession(t, f.origin)
	b.request("POST", "/login", url.Values{"username": {"alice"}, "password": {browserPassword}}, 303)
	sessions := New(f.store).sessions
	late, cookie := loadedBrowserSession(t, b, sessions)
	sessions.Put(late, "late-request", true)
	b.request("POST", "/logout", nil, 303)
	if _, _, err := sessions.Commit(late); err != nil {
		t.Fatal(err)
	}
	restoreBrowserCookie(b, cookie)
	b.request("GET", "/alice/project", nil, 303)
}

func TestBrowserKeyLoginAndPasswordVerification(t *testing.T) {
	f := apiTest(t)
	alice, old := newBrowserSession(t, f.origin), newBrowserSession(t, f.origin)
	for _, b := range []*browserSession{alice, old} {
		b.request("POST", "/login", url.Values{"username": {"alice"}, "password": {browserPassword}}, 303)
	}
	sessions := New(f.store).sessions
	late, cookie := loadedBrowserSession(t, old, sessions)
	sessions.Put(late, "late-request", true)
	key := newSyntheticAuthenticator(t, "browser-passkey")
	codes := enrollBrowserKey(t, alice, key, "passkey")
	if len(codes) != 10 {
		t.Fatal("first key did not return recovery codes")
	}
	alice.request("GET", "/settings/security", nil, 200)
	old.request("GET", "/alice/project", nil, 303)
	expectTokenStatus(t, f.origin, f.token, 401)
	if _, _, err := sessions.Commit(late); err != nil {
		t.Fatal(err)
	}
	restoreBrowserCookie(old, cookie)
	old.request("GET", "/admin/users", nil, 303)

	// A fresh session created late from password authentication before enrollment
	// must also be stale, even though its session token has never existed before.
	lateLogin, err := sessions.Load(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	sessions.Put(lateLogin, "user", f.user.ID)
	sessions.Put(lateLogin, "generation", f.user.Generation)
	staleToken, _, err := sessions.Commit(lateLogin)
	if err != nil {
		t.Fatal(err)
	}
	restoreBrowserCookie(old, &http.Cookie{Name: sessions.Cookie.Name, Value: staleToken, Path: "/"})
	old.request("GET", "/settings/security", nil, 303)

	pending := newBrowserSession(t, f.origin)
	pending.request("POST", "/login", url.Values{"username": {"alice"}, "password": {browserPassword}}, 303)
	pending.request("GET", "/verify", nil, 200)
	for _, path := range []string{"/alice/project", "/settings/security", "/settings/tokens", "/admin/users"} {
		pending.request("GET", path, nil, 303)
	}
	pending.request("POST", "/settings/tokens", url.Values{"name": {"forbidden"}, "role": {"writer"}}, 303)
	pending.request("POST", "/settings/security/keys", url.Values{"current": {browserPassword}, "name": {"forbidden"}, "mode": {"passkey"}}, 401)
	pending.request("POST", "/verify/recovery", url.Values{"code": {"incorrect"}}, 400)
	c := beginBrowserKey(t, pending, "/verify/key", nil)
	body := assertionResponse(t, key, c.Options.PublicKey.Challenge, f.origin, 1)
	pending.keyJSON("/verify/key/finish?id="+c.ID, body, 200)
	pending.request("GET", "/alice/project", nil, 200)
	pending.keyJSON("/verify/key/finish?id="+c.ID, body, 401)

	// Both passwordless paths exercise the same credential; only discovery needs
	// its opaque account handle. Use fresh browsers to prove no password session.
	for i, name := range []string{"", "alice"} {
		time.Sleep(time.Second) // both start and finish consume a sign-in attempt
		b := newBrowserSession(t, f.origin)
		c = beginBrowserKey(t, b, "/login/key", url.Values{"username": {name}})
		if (name == "") != (len(c.Options.PublicKey.AllowCredentials) == 0) {
			t.Fatal("incorrect discovery mode")
		}
		body = passkeyAssertionResponse(t, key, f.user.ID, c.Options.PublicKey.Challenge, f.origin, uint32(i+2))
		b.keyJSON("/login/key/finish?id="+c.ID, body, 200)
		b.request("GET", "/alice/project", nil, 200)
	}
	pending.request("POST", "/logout", nil, 303)
	time.Sleep(time.Second)
	pending.request("POST", "/login", url.Values{"username": {"alice"}, "password": {browserPassword}}, 303)
	pending.request("POST", "/verify/recovery", url.Values{"code": {codes[0]}}, 303)
	pending.request("GET", "/alice/project", nil, 200)
	pending.request("POST", "/logout", nil, 303)
	time.Sleep(time.Second)
	pending.request("POST", "/login", url.Values{"username": {"alice"}, "password": {browserPassword}}, 303)
	pending.request("POST", "/verify/recovery", url.Values{"code": {codes[0]}}, 400)
	pending.request("GET", "/alice/project", nil, 303)
	if page := alice.request("GET", "/settings/security", nil, 200); strings.Contains(page, codes[1]) {
		t.Fatal("recovery codes persisted in page")
	}
}

func TestBrowserRecoveryRotationAndPendingExpiry(t *testing.T) {
	f := apiTest(t)
	alice, pending := newBrowserSession(t, f.origin), newBrowserSession(t, f.origin)
	alice.request("POST", "/login", url.Values{"username": {"alice"}, "password": {browserPassword}}, 303)
	key := newSyntheticAuthenticator(t, "recovery-key")
	codes := enrollBrowserKey(t, alice, key, "username")
	pending.request("POST", "/login", url.Values{"username": {"alice"}, "password": {browserPassword}}, 303)
	sessions := New(f.store).sessions
	late, cookie := loadedBrowserSession(t, pending, sessions)
	sessions.Put(late, "late-request", true)
	page := alice.request("POST", "/settings/security/recovery", url.Values{"current": {browserPassword}}, 200)
	if strings.Contains(page, codes[0]) || !strings.Contains(page, "Save your recovery codes") {
		t.Fatal("recovery rotation failed")
	}
	alice.request("GET", "/settings/security", nil, 200)
	if _, _, err := sessions.Commit(late); err != nil {
		t.Fatal(err)
	}
	restoreBrowserCookie(pending, cookie)
	pending.request("POST", "/verify/recovery", url.Values{"code": {codes[0]}}, 303)
	pending.request("GET", "/alice/project", nil, 303)
	pending.request("POST", "/login", url.Values{"username": {"alice"}, "password": {browserPassword}}, 303)
	pending.request("POST", "/verify/recovery", url.Values{"code": {codes[0]}}, 400)
	expired, _ := loadedBrowserSession(t, pending, sessions)
	sessions.Put(expired, "pending-until", time.Now().Add(-time.Second).Unix())
	if _, _, err := sessions.Commit(expired); err != nil {
		t.Fatal(err)
	}
	pending.request("POST", "/verify/key", nil, 401)
	pending.request("GET", "/verify", nil, 303)

	// Removing the last key ends the password's second-factor requirement.
	keyID := base64.RawURLEncoding.EncodeToString(key.id)
	alice.request("POST", "/settings/security/keys/"+keyID+"/remove", url.Values{"current": {"wrong password"}}, 400)
	alice.request("POST", "/settings/security/keys/"+keyID+"/remove", url.Values{"current": {browserPassword}}, 303)
	alice.request("POST", "/settings/security/recovery", url.Values{"current": {browserPassword}}, 400)
	pending.request("POST", "/login", url.Values{"username": {"alice"}, "password": {browserPassword}}, 303)
	pending.request("GET", "/alice/project", nil, 200)
}

func TestBrowserCompletedRecoveryCannotOutliveAccountReset(t *testing.T) {
	f := apiTest(t)
	alice, pending := newBrowserSession(t, f.origin), newBrowserSession(t, f.origin)
	alice.request("POST", "/login", url.Values{"username": {"alice"}, "password": {browserPassword}}, 303)
	codes := enrollBrowserKey(t, alice, newSyntheticAuthenticator(t, "reset-browser-key"), "passkey")
	pending.request("POST", "/login", url.Values{"username": {"alice"}, "password": {browserPassword}}, 303)
	sessions := New(f.store).sessions
	ctx, _ := loadedBrowserSession(t, pending, sessions)
	user, err := New(f.store).sessionUser((&http.Request{}).WithContext(ctx), true)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := f.store.UseRecovery(context.Background(), user, codes[0])
	if err != nil {
		t.Fatal(err)
	}
	// Reset after proof verification but before the response creates its session.
	alice.request("POST", "/admin/users/alice/edit", url.Values{"generation": {accountGeneration(t, alice, "alice")}, "admin": {"1"}, "enabled": {"1"}, "password": {changedPassword}, "confirm": {changedPassword}}, 303)
	if err := New(f.store).startSession((&http.Request{}).WithContext(ctx), verified, false); err != nil {
		t.Fatal(err)
	}
	token, _, err := sessions.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	restoreBrowserCookie(pending, &http.Cookie{Name: sessions.Cookie.Name, Value: token, Path: "/"})
	pending.request("GET", "/alice/project", nil, 303)
	pending.request("POST", "/login", url.Values{"username": {"alice"}, "password": {changedPassword}}, 303)
	pending.request("GET", "/alice/project", nil, 200)
	if page := pending.request("GET", "/settings/security", nil, 200); strings.Contains(page, "reset-browser-key") {
		t.Fatal("reset retained key")
	}
}
