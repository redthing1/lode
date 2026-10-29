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
	"testing"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
)

const testOrigin = "http://localhost:8474"
const testRPID = "localhost"

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

func registerKey(t *testing.T, s *Store, user User, auth syntheticAuthenticator, discoverable bool) (User, []string) {
	t.Helper()
	ctx := context.Background()
	c, err := s.BeginRegistration(ctx, Principal{User: user}, "enrollment", originalPassword, "Workstation", discoverable)
	if err != nil {
		t.Fatal(err)
	}
	options := c.Options.(*protocol.CredentialCreation)
	want := protocol.ResidentKeyRequirementDiscouraged
	if discoverable {
		want = protocol.ResidentKeyRequirementRequired
	}
	if options.Response.AuthenticatorSelection.ResidentKey != want || options.Response.AuthenticatorSelection.UserVerification != protocol.VerificationRequired {
		t.Fatal("incorrect enrollment requirements")
	}
	u, codes, err := s.FinishRegistration(ctx, Principal{User: user}, "enrollment", c.ID, webauthnRequest(registrationResponse(t, auth, optionChallenge(t, c.Options, true), testOrigin)))
	if err != nil || u.Generation != user.Generation+1 {
		t.Fatalf("enrollment: %+v %v", u, err)
	}
	return u, codes
}

func TestKeyEnrollmentLoginAndRecovery(t *testing.T) {
	ctx := context.Background()
	s, oldUser := instanceFixture(t)
	token, err := s.CreateToken(ctx, oldUser, TokenOptions{Name: "old", Role: Writer})
	if err != nil {
		t.Fatal(err)
	}
	auth := newSyntheticAuthenticator(t, "primary-key")
	user, codes := registerKey(t, s, oldUser, auth, true)
	if len(codes) != 10 {
		t.Fatal("first enrollment did not produce recovery codes")
	}
	if _, err := s.Token(ctx, token); !errors.Is(err, ErrCredentials) {
		t.Fatalf("enrollment retained token: %v", err)
	}
	if _, err := s.CreateToken(ctx, oldUser, TokenOptions{Name: "late password login", Role: Reader}); !errors.Is(err, ErrCredentials) {
		t.Fatalf("old password login retained authority: %v", err)
	}
	keys, err := s.Keys(ctx, user)
	if err != nil || len(keys) != 1 || keys[0].Name != "Workstation" {
		t.Fatalf("keys: %+v %v", keys, err)
	}
	for i, username := range []string{user.Name, ""} {
		c, err := s.BeginKeyLogin(ctx, "sign-in", username)
		if err != nil {
			t.Fatal(err)
		}
		body := passkeyAssertionResponse(t, auth, user.ID, optionChallenge(t, c.Options, false), testOrigin, uint32(i+1))
		got, err := s.FinishKeyLogin(ctx, "sign-in", c.ID, webauthnRequest(body))
		if err != nil || got != user {
			t.Fatalf("key login %q: %+v %v", username, got, err)
		}
		if _, err := s.FinishKeyLogin(ctx, "sign-in", c.ID, webauthnRequest(body)); !errors.Is(err, ErrKey) {
			t.Fatalf("replayed challenge: %v", err)
		}
	}
	backup := newSyntheticAuthenticator(t, "backup-key")
	user, newCodes := registerKey(t, s, user, backup, false)
	if len(newCodes) != 0 {
		t.Fatal("additional key silently replaced recovery codes")
	}
	c, err := s.BeginVerification(ctx, user, "password-session")
	if err != nil {
		t.Fatal(err)
	}
	verified, err := s.FinishVerification(ctx, user, "password-session", c.ID, webauthnRequest(assertionResponse(t, backup, optionChallenge(t, c.Options, false), testOrigin, 1)))
	if err != nil || verified != user {
		t.Fatalf("password second factor: %+v %v", verified, err)
	}
	if got, err := s.UseRecovery(ctx, user, codes[0]); err != nil || got != user {
		t.Fatalf("recovery: %+v %v", got, err)
	}
	if _, err := s.UseRecovery(ctx, user, codes[0]); !errors.Is(err, ErrRecovery) {
		t.Fatalf("replayed recovery code: %v", err)
	}
	previous := user
	user, newCodes, err = s.RecoveryCodes(ctx, Principal{User: user}, originalPassword)
	if err != nil || user.Generation != previous.Generation+1 || len(newCodes) != 10 {
		t.Fatalf("regenerate recovery: %+v %v", user, err)
	}
	if _, err := s.UseRecovery(ctx, user, codes[1]); !errors.Is(err, ErrRecovery) {
		t.Fatalf("replaced code accepted: %v", err)
	}
	if _, err := s.UseRecovery(ctx, previous, newCodes[0]); !errors.Is(err, ErrCredentials) {
		t.Fatalf("stale pending password login: %v", err)
	}
	if _, err := s.RemoveKey(ctx, Principal{User: user}, "incorrect", keys[0].ID); !errors.Is(err, ErrCurrentPassword) {
		t.Fatalf("key removal without current password: %v", err)
	}
	user, err = s.RemoveKey(ctx, Principal{User: user}, originalPassword, keys[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UseRecovery(ctx, user, newCodes[0]); err != nil {
		t.Fatal("removing one key lost recovery codes", err)
	}
	user, err = s.RemoveKey(ctx, Principal{User: user}, originalPassword, base64.RawURLEncoding.EncodeToString(backup.id))
	if err != nil {
		t.Fatal(err)
	}
	if remaining, err := s.Keys(ctx, user); err != nil || len(remaining) != 0 {
		t.Fatalf("last key removal: %v %v", remaining, err)
	}
	if _, err := s.UseRecovery(ctx, user, newCodes[1]); !errors.Is(err, ErrRecovery) {
		t.Fatalf("last key removal retained recovery: %v", err)
	}
}

func TestKeyChallengeBindingOriginExpiryAndPurpose(t *testing.T) {
	ctx := context.Background()
	s, user := instanceFixture(t)
	auth := newSyntheticAuthenticator(t, "bound-key")
	c, err := s.BeginRegistration(ctx, Principal{User: user}, "browser-a", originalPassword, "Bound key", true)
	if err != nil {
		t.Fatal(err)
	}
	body := registrationResponse(t, auth, optionChallenge(t, c.Options, true), testOrigin)
	if _, _, err := s.FinishRegistration(ctx, Principal{User: user}, "browser-b", c.ID, webauthnRequest(body)); !errors.Is(err, ErrKey) {
		t.Fatalf("cross-browser registration: %v", err)
	}
	user, _, err = s.FinishRegistration(ctx, Principal{User: user}, "browser-a", c.ID, webauthnRequest(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"origin", "expiry", "purpose", "handle"} {
		t.Run(mode, func(t *testing.T) {
			c, err := s.BeginKeyLogin(ctx, "browser-a", "")
			if err != nil {
				t.Fatal(err)
			}
			origin, handle := testOrigin, user.ID
			if mode == "origin" {
				origin = "https://attacker.example"
			}
			if mode == "handle" {
				handle = "another-account"
			}
			if mode == "expiry" {
				if _, err := s.db.Exec("UPDATE challenges SET expires=0 WHERE id=?", c.ID); err != nil {
					t.Fatal(err)
				}
			}
			body := passkeyAssertionResponse(t, auth, handle, optionChallenge(t, c.Options, false), origin, 1)
			if mode == "purpose" {
				_, err = s.FinishVerification(ctx, user, "browser-a", c.ID, webauthnRequest(body))
			} else {
				_, err = s.FinishKeyLogin(ctx, "browser-a", c.ID, webauthnRequest(body))
			}
			if !errors.Is(err, ErrKey) {
				t.Fatalf("invalid challenge accepted: %v", err)
			}
		})
	}
	c, err = s.BeginKeyLogin(ctx, "browser-a", user.Name)
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := s.BeginKeyLogin(ctx, "browser-a", "unknown-user")
	if err != nil {
		t.Fatal(err)
	}
	for _, challenge := range []Ceremony{c, unknown} {
		body := passkeyAssertionResponse(t, auth, user.ID, optionChallenge(t, challenge.Options, false), testOrigin, 1)
		if _, err := s.FinishKeyLogin(ctx, "browser-a", challenge.ID, webauthnRequest(body)); !errors.Is(err, ErrKey) {
			t.Fatalf("obsolete or unknown-user challenge: %v", err)
		}
	}
}

func TestRecoveryCodeConsumptionIsAtomic(t *testing.T) {
	ctx := context.Background()
	s, user := instanceFixture(t)
	user, codes := registerKey(t, s, user, newSyntheticAuthenticator(t, "recovery-key"), true)
	done := make(chan error, 2)
	for range 2 {
		go func() { _, err := s.UseRecovery(ctx, user, codes[0]); done <- err }()
	}
	successes := 0
	for range 2 {
		err := <-done
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrRecovery) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful recovery attempts: %d", successes)
	}
}

type pausedAssertion struct {
	*bytes.Reader
	entered, release chan struct{}
	paused           bool
}

func (b *pausedAssertion) Read(p []byte) (int, error) {
	if !b.paused {
		b.paused = true
		close(b.entered)
		<-b.release
	}
	return b.Reader.Read(p)
}
func (b *pausedAssertion) Close() error { return nil }

func TestPasswordChangeDuringKeyAssertion(t *testing.T) {
	ctx := context.Background()
	s, user := instanceFixture(t)
	auth := newSyntheticAuthenticator(t, "race-key")
	user, _ = registerKey(t, s, user, auth, true)
	c, err := s.BeginKeyLogin(ctx, "race-browser", user.Name)
	if err != nil {
		t.Fatal(err)
	}
	payload := assertionResponse(t, auth, optionChallenge(t, c.Options, false), testOrigin, 1)
	body := &pausedAssertion{Reader: bytes.NewReader(payload), entered: make(chan struct{}), release: make(chan struct{})}
	r := webauthnRequest(nil)
	r.Body = body
	done := make(chan error, 1)
	go func() { _, err := s.FinishKeyLogin(ctx, "race-browser", c.ID, r); done <- err }()
	<-body.entered // The known-account flow has loaded its credential and generation.
	err = s.ChangePassword(ctx, Principal{User: user}, originalPassword, replacementPassword)
	close(body.release)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrKey) {
		t.Fatalf("stale assertion accepted after password change: %v", err)
	}
	user, err = s.Password(ctx, user.Name, replacementPassword)
	if err != nil {
		t.Fatal(err)
	}
	c, err = s.BeginKeyLogin(ctx, "new-browser", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.FinishKeyLogin(ctx, "new-browser", c.ID, webauthnRequest(passkeyAssertionResponse(t, auth, user.ID, optionChallenge(t, c.Options, false), testOrigin, 1)))
	if err != nil || got != user {
		t.Fatalf("voluntary password change lost enrolled key: %+v %v", got, err)
	}
}

func TestKeyLoginRequiresUserVerificationAndCurrentCounter(t *testing.T) {
	ctx := context.Background()
	s, user := instanceFixture(t)
	auth := newSyntheticAuthenticator(t, "verified-key")
	user, _ = registerKey(t, s, user, auth, false)
	for i, flags := range []byte{byte(protocol.FlagUserPresent), byte(protocol.FlagUserPresent | protocol.FlagUserVerified), byte(protocol.FlagUserPresent | protocol.FlagUserVerified)} {
		c, err := s.BeginKeyLogin(ctx, "browser", user.Name)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.FinishKeyLogin(ctx, "browser", c.ID, webauthnRequest(assertionWithFlags(t, auth, optionChallenge(t, c.Options, false), testOrigin, 1, flags)))
		if i == 1 && err != nil {
			t.Fatal("verified assertion rejected", err)
		}
		if i != 1 && !errors.Is(err, ErrKey) {
			t.Fatalf("unverified or repeated counter accepted: %v", err)
		}
	}
}

func TestKeyEnrollmentRollbackAndAccountReset(t *testing.T) {
	ctx := context.Background()
	s, user := instanceFixture(t)
	token, err := s.CreateToken(ctx, user, TokenOptions{Name: "kept", Role: Reader})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER refuse_key_invalidation BEFORE DELETE ON tokens BEGIN SELECT RAISE(ABORT,'injected revocation failure'); END;`); err != nil {
		t.Fatal(err)
	}
	auth := newSyntheticAuthenticator(t, "reset-key")
	c, err := s.BeginRegistration(ctx, Principal{User: user}, "register", originalPassword, "Key", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FinishRegistration(ctx, Principal{User: user}, "register", c.ID, webauthnRequest(registrationResponse(t, auth, optionChallenge(t, c.Options, true), testOrigin))); err == nil {
		t.Fatal("partial enrollment accepted")
	}
	if keys, err := s.Keys(ctx, user); err != nil || len(keys) != 0 {
		t.Fatalf("key/generation survived failed enrollment: %v %v", keys, err)
	}
	if _, err := s.Token(ctx, token); err != nil {
		t.Fatal("failed enrollment revoked token", err)
	}
	if _, err := s.db.Exec("DROP TRIGGER refuse_key_invalidation"); err != nil {
		t.Fatal(err)
	}
	user, codes := registerKey(t, s, user, auth, true)
	c, err = s.BeginKeyLogin(ctx, "anonymous", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUser(ctx, Principal{User: user}, user.ID, AccountUpdate{Generation: user.Generation, Admin: true, Password: replacementPassword}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishKeyLogin(ctx, "anonymous", c.ID, webauthnRequest(passkeyAssertionResponse(t, auth, user.ID, optionChallenge(t, c.Options, false), testOrigin, 1))); !errors.Is(err, ErrKey) {
		t.Fatalf("anonymous login survived full reset: %v", err)
	}
	user, err = s.Password(ctx, user.Name, replacementPassword)
	if err != nil {
		t.Fatal(err)
	}
	if keys, err := s.Keys(ctx, user); err != nil || len(keys) != 0 {
		t.Fatalf("reset retained keys: %v %v", keys, err)
	}
	if _, err := s.UseRecovery(ctx, user, codes[0]); !errors.Is(err, ErrRecovery) {
		t.Fatalf("reset retained recovery codes: %v", err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, s.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(ctx)
	if keys, err := reopened.Keys(ctx, user); err != nil || len(keys) != 0 {
		t.Fatalf("reopen retained reset keys: %v %v", keys, err)
	}
}

func TestConcurrentKeyCeremonyFinishes(t *testing.T) {
	ctx := context.Background()
	s, user := instanceFixture(t)
	auth := newSyntheticAuthenticator(t, "concurrent-key")
	c, err := s.BeginRegistration(ctx, Principal{User: user}, "browser", originalPassword, "Key", true)
	if err != nil {
		t.Fatal(err)
	}
	body := registrationResponse(t, auth, optionChallenge(t, c.Options, true), testOrigin)
	finishes := make(chan error, 2)
	for range 2 {
		go func() {
			_, _, err := s.FinishRegistration(ctx, Principal{User: user}, "browser", c.ID, webauthnRequest(body))
			finishes <- err
		}()
	}
	successes := 0
	for range 2 {
		if err := <-finishes; err == nil {
			successes++
		} else if !errors.Is(err, ErrKey) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful registrations: %d", successes)
	}
	c, err = s.BeginKeyLogin(ctx, "browser", "")
	if err != nil {
		t.Fatal(err)
	}
	body = passkeyAssertionResponse(t, auth, user.ID, optionChallenge(t, c.Options, false), testOrigin, 1)
	for range 2 {
		go func() { _, err := s.FinishKeyLogin(ctx, "browser", c.ID, webauthnRequest(body)); finishes <- err }()
	}
	successes = 0
	for range 2 {
		if err := <-finishes; err == nil {
			successes++
		} else if !errors.Is(err, ErrKey) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful assertions: %d", successes)
	}
}

func TestOperatorRecoveryPreservesOwnershipAndClearsFactors(t *testing.T) {
	ctx := context.Background()
	s, admin := instanceFixture(t)
	user, err := s.CreateUser(ctx, Principal{User: admin}, "bob", originalPassword)
	if err != nil {
		t.Fatal(err)
	}
	user, codes := registerKey(t, s, user, newSyntheticAuthenticator(t, "operator-key"), true)
	owned, err := s.CreateProject(ctx, Principal{User: user}, "kept")
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateToken(ctx, user, TokenOptions{Name: "old", Role: Reader})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER refuse_reset BEFORE DELETE ON security_keys BEGIN SELECT RAISE(ABORT,'injected reset failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverAccount(ctx, user.Name, replacementPassword); err == nil {
		t.Fatal("partial recovery accepted")
	}
	if got, err := s.Password(ctx, user.Name, originalPassword); err != nil || got != user {
		t.Fatal("failed reset changed password/generation", err)
	}
	if _, err := s.Token(ctx, token); err != nil {
		t.Fatal("failed reset revoked token", err)
	}
	if _, err := s.db.Exec("DROP TRIGGER refuse_reset"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUser(ctx, Principal{User: admin}, user.ID, AccountUpdate{Generation: user.Generation, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverAccount(ctx, user.Name, replacementPassword); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.Password(ctx, user.Name, replacementPassword)
	if err != nil || recovered.Disabled || recovered.Admin || recovered.ID != user.ID || recovered.Generation != user.Generation+2 {
		t.Fatalf("account recovery: %+v %v", recovered, err)
	}
	if _, err := s.UseRecovery(ctx, recovered, codes[0]); !errors.Is(err, ErrRecovery) {
		t.Fatal("recovery code survived reset", err)
	}
	if keys, err := s.Keys(ctx, recovered); err != nil || len(keys) != 0 {
		t.Fatal("keys survived reset", err)
	}
	if _, err := s.Token(ctx, token); !errors.Is(err, ErrCredentials) {
		t.Fatal("token survived reset", err)
	}
	if got, err := s.Project(ctx, Principal{User: recovered}, "bob", "kept", Maintainer); err != nil || got.ID != owned.ID {
		t.Fatal("project ownership lost", err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, s.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(ctx)
	if got, err := reopened.Password(ctx, user.Name, replacementPassword); err != nil || got != recovered {
		t.Fatal("reopened recovery", err)
	}
	if keys, err := reopened.Keys(ctx, recovered); err != nil || len(keys) != 0 {
		t.Fatal("reopened factors", err)
	}
}
