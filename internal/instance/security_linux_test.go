//go:build linux

package instance

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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
)

const testOrigin = "https://example.com"
const testRPID = "example.com"

func securityFixture(t *testing.T) (context.Context, *Store, *Security, User) {
	t.Helper()
	ctx := context.Background()
	store := testStore(t)
	user, err := store.CreateUser(ctx, "security-user", "a sufficiently long password", false)
	if err != nil {
		t.Fatal(err)
	}
	user, err = store.SetPassword(ctx, user.ID, "a sufficiently long password")
	if err != nil {
		t.Fatal(err)
	}
	security, err := NewSecurity(store, testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, store, security, user
}

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
	authData := make([]byte, 0, 37)
	rpHash := sha256.Sum256([]byte(testRPID))
	authData = append(authData, rpHash[:]...)
	authData = append(authData, byte(protocol.FlagUserPresent|protocol.FlagUserVerified))
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

// The response builders below follow the public WebAuthn wire formats. The COSE, authenticator-data,
// and signed-client-data construction is adapted from go-webauthn/webauthn v0.18.2's e2e fixtures.
func webauthnRequest(body []byte) *http.Request {
	return httptest.NewRequest("POST", testOrigin+"/webauthn", bytes.NewReader(body))
}

func optionChallenge(t *testing.T, options any, registration bool) string {
	t.Helper()
	if registration {
		opts, ok := options.(*protocol.CredentialCreation)
		if !ok {
			t.Fatalf("registration options type %T", options)
		}
		return opts.Response.Challenge.String()
	}
	opts, ok := options.(*protocol.CredentialAssertion)
	if !ok {
		t.Fatalf("assertion options type %T", options)
	}
	return opts.Response.Challenge.String()
}

func registerSynthetic(t *testing.T, ctx context.Context, security *Security, user User, auth syntheticAuthenticator, binding string) []string {
	t.Helper()
	id, options, err := security.Begin(ctx, user.ID, binding, "register")
	if err != nil {
		t.Fatal(err)
	}
	codes, err := security.FinishRegistration(ctx, user.ID, binding, id, "test key", webauthnRequest(registrationResponse(t, auth, optionChallenge(t, options, true), testOrigin)))
	if err != nil {
		t.Fatal(err)
	}
	return codes
}

func TestSecurityRegistrationBindingOneUseExpiryAndOrigin(t *testing.T) {
	ctx, store, security, user := securityFixture(t)
	auth := newSyntheticAuthenticator(t, "registration-key-0001")
	id, options, err := security.Begin(ctx, user.ID, "browser-session-a", "register")
	if err != nil {
		t.Fatal(err)
	}
	creation := options.(*protocol.CredentialCreation).Response
	if creation.AuthenticatorSelection.ResidentKey != protocol.ResidentKeyRequirementRequired || creation.AuthenticatorSelection.RequireResidentKey == nil || !*creation.AuthenticatorSelection.RequireResidentKey || creation.AuthenticatorSelection.UserVerification != protocol.VerificationRequired {
		t.Fatalf("registration did not require a discoverable key with UV: %+v", creation.AuthenticatorSelection)
	}
	body := registrationResponse(t, auth, optionChallenge(t, options, true), testOrigin)
	if _, err = security.FinishRegistration(ctx, user.ID, "browser-session-b", id, "test key", webauthnRequest(body)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong binding: %v", err)
	}
	codes, err := security.FinishRegistration(ctx, user.ID, "browser-session-a", id, "test key", webauthnRequest(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 10 {
		t.Fatalf("got %d recovery codes", len(codes))
	}
	if _, err = security.FinishRegistration(ctx, user.ID, "browser-session-a", id, "test key", webauthnRequest(body)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("reused registration challenge: %v", err)
	}
	keys, err := security.Keys(ctx, user.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("registered keys=%d err=%v", len(keys), err)
	}

	badID, badOptions, err := security.Begin(ctx, user.ID, "browser-session-a", "register")
	if err != nil {
		t.Fatal(err)
	}
	badAuth := newSyntheticAuthenticator(t, "registration-key-0002")
	badBody := registrationResponse(t, badAuth, optionChallenge(t, badOptions, true), "https://attacker.example")
	if _, err = security.FinishRegistration(ctx, user.ID, "browser-session-a", badID, "bad origin", webauthnRequest(badBody)); err == nil {
		t.Fatal("accepted registration from invalid origin")
	}
	if _, err = security.FinishRegistration(ctx, user.ID, "browser-session-a", badID, "bad origin", webauthnRequest(registrationResponse(t, badAuth, optionChallenge(t, badOptions, true), testOrigin))); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("invalid-origin challenge was not consumed: %v", err)
	}

	expiredID, expiredOptions, err := security.Begin(ctx, user.ID, "browser-session-a", "register")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.ExecContext(ctx, `UPDATE ceremonies SET expires=? WHERE id=?`, time.Now().Add(-time.Second).Unix(), expiredID); err != nil {
		t.Fatal(err)
	}
	expiredAuth := newSyntheticAuthenticator(t, "registration-key-0003")
	if _, err = security.FinishRegistration(ctx, user.ID, "browser-session-a", expiredID, "expired", webauthnRequest(registrationResponse(t, expiredAuth, optionChallenge(t, expiredOptions, true), testOrigin))); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired registration challenge: %v", err)
	}
}

func TestUsernameCompatibleEnrollmentOmitsResidentKeyButRequiresUV(t *testing.T) {
	ctx, _, security, user := securityFixture(t)
	_, options, err := security.BeginWithMode(ctx, user.ID, "browser", "register", "username")
	if err != nil {
		t.Fatal(err)
	}
	selection := options.(*protocol.CredentialCreation).Response.AuthenticatorSelection
	if selection.UserVerification != protocol.VerificationRequired || selection.ResidentKey != protocol.ResidentKeyRequirementDiscouraged || selection.RequireResidentKey == nil || *selection.RequireResidentKey {
		t.Fatalf("username-compatible enrollment options %+v", selection)
	}
	if _, _, err := security.BeginWithMode(ctx, user.ID, "browser", "register", "unknown"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown enrollment mode returned %v", err)
	}
}

func TestSecurityAssertionSignatureCounterBindingAndReplay(t *testing.T) {
	ctx, _, security, user := securityFixture(t)
	auth := newSyntheticAuthenticator(t, "assertion-key-0001")
	registerSynthetic(t, ctx, security, user, auth, "browser-session-a")
	id, options, err := security.Begin(ctx, user.ID, "browser-session-a", "login")
	if err != nil {
		t.Fatal(err)
	}
	body := assertionResponse(t, auth, optionChallenge(t, options, false), testOrigin, 1)
	if err = security.FinishLogin(ctx, user.ID, "other-session", id, webauthnRequest(body)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong assertion binding: %v", err)
	}
	if err = security.FinishLogin(ctx, user.ID, "browser-session-a", id, webauthnRequest(body)); err != nil {
		t.Fatalf("valid ECDSA assertion: %v", err)
	}
	if err = security.FinishLogin(ctx, user.ID, "browser-session-a", id, webauthnRequest(body)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("assertion challenge replay: %v", err)
	}

	id, options, err = security.Begin(ctx, user.ID, "browser-session-a", "login")
	if err != nil {
		t.Fatal(err)
	}
	if err = security.FinishLogin(ctx, user.ID, "browser-session-a", id, webauthnRequest(assertionResponse(t, auth, optionChallenge(t, options, false), "https://attacker.example", 2))); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("invalid assertion origin: %v", err)
	}

	id, options, err = security.Begin(ctx, user.ID, "browser-session-a", "login")
	if err != nil {
		t.Fatal(err)
	}
	if err = security.FinishLogin(ctx, user.ID, "browser-session-a", id, webauthnRequest(assertionResponse(t, auth, optionChallenge(t, options, false), testOrigin, 2))); err != nil {
		t.Fatalf("second valid assertion: %v", err)
	}

	id, options, err = security.Begin(ctx, user.ID, "browser-session-a", "login")
	if err != nil {
		t.Fatal(err)
	}
	if err = security.FinishLogin(ctx, user.ID, "browser-session-a", id, webauthnRequest(assertionResponse(t, auth, optionChallenge(t, options, false), testOrigin, 2))); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("replayed authenticator counter accepted: %v", err)
	}

	expiredID, expiredOptions, err := security.Begin(ctx, user.ID, "browser-session-a", "login")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = security.store.db.ExecContext(ctx, `UPDATE ceremonies SET expires=? WHERE id=?`, time.Now().Add(-time.Second).Unix(), expiredID); err != nil {
		t.Fatal(err)
	}
	if err = security.FinishLogin(ctx, user.ID, "browser-session-a", expiredID, webauthnRequest(assertionResponse(t, auth, optionChallenge(t, expiredOptions, false), testOrigin, 3))); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired assertion: %v", err)
	}

	badAuth := newSyntheticAuthenticator(t, "assertion-key-0002")
	id, options, err = security.Begin(ctx, user.ID, "browser-session-a", "login")
	if err != nil {
		t.Fatal(err)
	}
	if err = security.FinishLogin(ctx, user.ID, "browser-session-a", id, webauthnRequest(assertionResponse(t, badAuth, optionChallenge(t, options, false), testOrigin, 3))); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("assertion signed by other key: %v", err)
	}
}

func TestDiscoverablePasskeyLoginRequiresUVAndConsumesChallenge(t *testing.T) {
	ctx, _, security, user := securityFixture(t)
	auth := newSyntheticAuthenticator(t, "discoverable-passkey-key")
	registerSynthetic(t, ctx, security, user, auth, "enrollment-session")
	keys, err := security.Keys(ctx, user.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("registered keys %d, error %v", len(keys), err)
	}
	binding := "anonymous-browser-session"
	id, options, err := security.BeginPasskey(ctx, binding, "")
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		PublicKey struct {
			UserVerification string `json:"userVerification"`
			AllowCredentials []any  `json:"allowCredentials"`
		} `json:"publicKey"`
	}
	data, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.PublicKey.UserVerification != "required" || len(wire.PublicKey.AllowCredentials) != 0 {
		t.Fatalf("passkey options were not discoverable with required UV: %s", data)
	}
	challenge := optionChallenge(t, options, false)
	req := webauthnRequest(passkeyAssertionResponse(t, auth, user.ID, challenge, testOrigin, 7))
	loggedIn, err := security.FinishPasskey(ctx, binding, id, req)
	if err != nil || loggedIn.ID != user.ID {
		t.Fatalf("passkey sign-in returned user %q, error %v", loggedIn.ID, err)
	}
	if _, err := security.FinishPasskey(ctx, binding, id, webauthnRequest(passkeyAssertionResponse(t, auth, user.ID, challenge, testOrigin, 8))); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("replayed passkey ceremony returned %v", err)
	}
	keys, err = security.Keys(ctx, user.ID)
	if err != nil || keys[0].Credential.Authenticator.SignCount != 7 {
		t.Fatalf("credential counter was not persisted: %+v, %v", keys, err)
	}
}

func TestRecoveryCodesOneUseRegenerationAndLastKeyRemoval(t *testing.T) {
	ctx, store, security, user := securityFixture(t)
	// WebAuthn permits credential IDs up to 1023 bytes, including removal.
	auth := newSyntheticAuthenticator(t, strings.Repeat("k", 1023))
	codes := registerSynthetic(t, ctx, security, user, auth, "browser-session-a")
	if err := security.UseRecovery(ctx, user.ID, codes[0]); err != nil {
		t.Fatal(err)
	}
	if err := security.UseRecovery(ctx, user.ID, codes[0]); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("reused recovery code: %v", err)
	}
	beforeRecovery, err := store.GetUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	regenerated, err := security.Recovery(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(regenerated) != 10 {
		t.Fatalf("regenerated code count %d", len(regenerated))
	}
	afterRecovery, err := store.GetUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterRecovery.AuthVersion != beforeRecovery.AuthVersion+1 {
		t.Fatalf("recovery regeneration auth version %d -> %d", beforeRecovery.AuthVersion, afterRecovery.AuthVersion)
	}
	if err = security.UseRecovery(ctx, user.ID, codes[1]); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("old recovery code survived regeneration: %v", err)
	}
	before, err := store.GetUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := security.Keys(ctx, user.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys=%d err=%v", len(keys), err)
	}
	if err = security.RemoveKey(ctx, user.ID, keys[0].ID); err != nil {
		t.Fatal(err)
	}
	after, err := store.GetUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.AuthVersion != before.AuthVersion+1 {
		t.Fatalf("key removal auth version %d -> %d", before.AuthVersion, after.AuthVersion)
	}
	if err = security.UseRecovery(ctx, user.ID, regenerated[0]); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("last-key removal retained recovery codes: %v", err)
	}
	keys, err = security.Keys(ctx, user.ID)
	if err != nil || len(keys) != 0 {
		t.Fatalf("keys after last removal=%d err=%v", len(keys), err)
	}
	if _, err = security.Recovery(ctx, user.ID); err == nil {
		t.Fatal("generated recovery codes without a key")
	}
}

func TestSecurityRequiresCanonicalSecureOrigin(t *testing.T) {
	store := testStore(t)
	for _, origin := range []string{"http://example.com", "https://example.com/path", "https://example.com?x=1", "https://user@example.com", "https://EXAMPLE.com"} {
		if _, err := NewSecurity(store, origin); err == nil {
			t.Errorf("accepted invalid WebAuthn origin %q", origin)
		}
	}
	if _, err := NewSecurity(store, "http://localhost:8080"); err != nil {
		t.Fatalf("localhost HTTP origin: %v", err)
	}
}
