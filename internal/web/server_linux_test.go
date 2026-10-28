//go:build linux

package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/remote"
	"github.com/redthing1/lode/internal/transfer"
)

type browser struct {
	origin string
	client *http.Client
}

type browserResponse struct {
	status int
	header http.Header
	body   string
}

func newBrowser(t *testing.T, origin string) *browser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &browser{origin: origin, client: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (b *browser) request(path, method string, body []byte, contentType, origin, host string) browserResponse {
	request, err := http.NewRequest(method, b.origin+path, bytes.NewReader(body))
	if err != nil {
		panic(err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
		fetchSite := "cross-site"
		if origin == b.origin {
			fetchSite = "same-origin"
		}
		request.Header.Set("Sec-Fetch-Site", fetchSite)
	}
	if host != "" {
		request.Host = host
	}
	response, err := b.client.Do(request)
	if err != nil {
		panic(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		panic(err)
	}
	return browserResponse{status: response.StatusCode, header: response.Header.Clone(), body: string(data)}
}

func (b *browser) get(path string) browserResponse {
	return b.request(path, http.MethodGet, nil, "", "", "")
}
func (b *browser) post(path string, fields url.Values) browserResponse {
	return b.request(path, http.MethodPost, []byte(fields.Encode()), "application/x-www-form-urlencoded", b.origin, "")
}
func (b *browser) postFrom(path string, fields url.Values, origin string) browserResponse {
	return b.request(path, http.MethodPost, []byte(fields.Encode()), "application/x-www-form-urlencoded", origin, "")
}

func testServer(t *testing.T) (*instance.Instance, string, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	origin := fmt.Sprintf("http://localhost:%d", port)
	ctx := context.Background()
	inst, setupCode, err := instance.Create(ctx, filepath.Join(t.TempDir(), "instance"), origin)
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	handler, err := New(inst)
	if err != nil {
		_ = listener.Close()
		_ = inst.Close()
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	t.Cleanup(func() {
		if err := inst.Close(); err != nil {
			t.Errorf("close instance: %v", err)
		}
	})
	t.Cleanup(server.Close)
	return inst, origin, setupCode
}

var oneTimeValue = regexp.MustCompile(`<code data-copy-value>([^<]+)</code>`)
var renderedAnchor = regexp.MustCompile(`(?s)<a\b[^>]*href="([^"]+)"[^>]*>(.*?)</a\s*>`)
var htmlTag = regexp.MustCompile(`<[^>]*>`)
var selectedWorkBranch = regexp.MustCompile(`<option\b[^>]*value="work"[^>]*selected`)

func anchorWithText(t *testing.T, body, text string) string {
	t.Helper()
	for _, match := range renderedAnchor.FindAllStringSubmatch(body, -1) {
		label := strings.TrimSpace(html.UnescapeString(htmlTag.ReplaceAllString(match[2], "")))
		if label == text {
			return html.UnescapeString(match[1])
		}
	}
	t.Fatalf("rendered link %q not found", text)
	return ""
}

func secretFromPage(t *testing.T, body string) string {
	t.Helper()
	match := oneTimeValue.FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("one-time secret not present in page: %s", body)
	}
	return match[1]
}

func postFields(values ...string) url.Values {
	result := make(url.Values)
	for i := 0; i+1 < len(values); i += 2 {
		result.Set(values[i], values[i+1])
	}
	return result
}

func wantStatus(t *testing.T, response browserResponse, status int) {
	t.Helper()
	if response.status != status {
		t.Fatalf("status %d, want %d; response: %s", response.status, status, response.body)
	}
}

func fileEntry(name string, data []byte) model.Entry {
	hash := sha256.Sum256(data)
	return model.Entry{Path: name, Kind: model.File, Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}
}

func saveFile(t *testing.T, ctx context.Context, client *remote.Client, branch, base, author, message string, changes []model.Entry, contents map[string][]byte) project.Commit {
	t.Helper()
	commit, err := client.Save(ctx, project.Request{ID: project.NewID(), Branch: branch, Base: base, Author: author, Message: message, Changes: changes}, func(name string) (io.ReadCloser, error) {
		data, ok := contents[name]
		if !ok {
			return nil, errors.New("unexpected requested payload")
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return commit
}

func TestHostedHTTPJourneyAndAuthorization(t *testing.T) {
	ctx := context.Background()
	inst, origin, setupCode := testServer(t)
	admin := newBrowser(t, origin)

	// The configured address is enforced before route dispatch, and setup is
	// protected from cross-site POSTs before the one-time code is consumed.
	badHost := admin.request("/setup", http.MethodGet, nil, "", "", "attacker.example")
	wantStatus(t, badHost, http.StatusMisdirectedRequest)
	wantStatus(t, admin.get("/setup"), http.StatusOK)
	wantStatus(t, admin.postFrom("/setup", postFields("code", "wrong", "username", "owner", "password", "a long enough test password"), "https://attacker.example"), http.StatusForbidden)
	wantStatus(t, admin.post("/setup", postFields("code", "wrong", "username", "owner", "password", "a long enough test password")), http.StatusBadRequest)
	if needed, err := inst.SetupRequired(ctx); err != nil || !needed {
		t.Fatalf("invalid code consumed setup: required=%v err=%v", needed, err)
	}
	if users, err := inst.ListUsers(ctx); err != nil || len(users) != 0 {
		t.Fatalf("invalid code created users: %d err=%v", len(users), err)
	}
	setup := admin.post("/setup", postFields("code", setupCode, "username", "owner", "password", "a long enough test password"))
	wantStatus(t, setup, http.StatusSeeOther)
	if setup.header.Get("Location") != "/settings/security?welcome=1" {
		t.Fatalf("setup redirected to %q", setup.header.Get("Location"))
	}
	if needed, err := inst.SetupRequired(ctx); err != nil || needed {
		t.Fatalf("setup code was not consumed: required=%v err=%v", needed, err)
	}
	if page := admin.get("/settings/security"); page.status != http.StatusOK || !strings.Contains(page.body, "Account security") {
		t.Fatalf("admin session missing after setup: %d %s", page.status, page.body)
	}
	wantStatus(t, admin.get("/setup"), http.StatusSeeOther)
	wantStatus(t, admin.get("/admin/users"), http.StatusOK)

	// The administrator creates two projects, a member, and explicit access.
	wantStatus(t, admin.post("/projects/new", postFields("slug", "binary-artifacts")), http.StatusSeeOther)
	wantStatus(t, admin.post("/projects/new", postFields("slug", "private-notes")), http.StatusSeeOther)
	firstProject, err := inst.Project(ctx, "binary-artifacts")
	if err != nil {
		t.Fatal(err)
	}
	secondProject, err := inst.Project(ctx, "private-notes")
	if err != nil {
		t.Fatal(err)
	}
	createUser := admin.post("/admin/users", postFields("username", "sam"))
	wantStatus(t, createUser, http.StatusOK)
	temporaryPassword := secretFromPage(t, createUser.body)
	adminAgain := admin.get("/admin/users")
	wantStatus(t, adminAgain, http.StatusOK)
	if strings.Contains(adminAgain.body, temporaryPassword) {
		t.Fatal("temporary password was shown again")
	}
	wantStatus(t, admin.post("/p/binary-artifacts/members", postFields("username", "sam", "role", "writer")), http.StatusSeeOther)
	adminTokenPage := admin.post("/settings/tokens", postFields("name", "instance-maintenance", "project", firstProject.ID, "role", "maintainer", "days", "90"))
	wantStatus(t, adminTokenPage, http.StatusOK)
	adminToken := secretFromPage(t, adminTokenPage.body)
	if page := admin.get("/settings/tokens"); page.status != http.StatusOK || strings.Contains(page.body, adminToken) {
		t.Fatalf("token was not one-time: %d %s", page.status, page.body)
	}

	member := newBrowser(t, origin)
	login := member.post("/login", postFields("username", "sam", "password", temporaryPassword))
	wantStatus(t, login, http.StatusSeeOther)
	if login.header.Get("Location") != "/settings/security" {
		t.Fatalf("forced password change redirected to %q", login.header.Get("Location"))
	}
	wantStatus(t, member.get("/p/binary-artifacts"), http.StatusSeeOther)
	changePassword := member.post("/settings/password", postFields("current", temporaryPassword, "password", "sam's permanent passphrase", "confirm", "sam's permanent passphrase"))
	wantStatus(t, changePassword, http.StatusSeeOther)
	if changePassword.header.Get("Location") != "/settings/security?changed=1" {
		t.Fatalf("password change redirected to %q", changePassword.header.Get("Location"))
	}
	memberUser, err := inst.LookupUser(ctx, "sam")
	if err != nil || memberUser.ForcePasswordChange {
		t.Fatalf("temporary password was not cleared: %+v %v", memberUser, err)
	}
	wantStatus(t, member.get("/"), http.StatusOK)

	// A separate browser session is invalidated when the member changes their
	// password. Keep it for the stale-session assertion below.
	sibling := newBrowser(t, origin)
	wantStatus(t, sibling.post("/login", postFields("username", "sam", "password", "sam's permanent passphrase")), http.StatusSeeOther)

	memberTokenPage := member.post("/settings/tokens", postFields("name", "sam-laptop", "project", firstProject.ID, "role", "writer", "days", "90"))
	wantStatus(t, memberTokenPage, http.StatusOK)
	memberToken := secretFromPage(t, memberTokenPage.body)
	if strings.Contains(member.get("/settings/tokens").body, memberToken) {
		t.Fatal("personal token was shown after its creation response")
	}
	tokens, err := inst.ListTokens(ctx, memberUser.ID)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("member tokens=%d err=%v", len(tokens), err)
	}

	client, err := remote.New(origin+"/p/binary-artifacts", memberToken, firstProject.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := client.Info(ctx); err != nil || info.ID != firstProject.ID {
		t.Fatalf("scoped CLI info: %+v %v", info, err)
	}
	if head, err := client.Head(ctx, "main"); err != nil || head != "" {
		t.Fatalf("empty branch head %q: %v", head, err)
	}
	one := []byte("opaque binary contents, first revision\x00\xff")
	entryOne := fileEntry("assets.bin", one)
	message := `Release <script>alert("unsafe")</script>`
	commitOne := saveFile(t, ctx, client, "main", "", "attacker claimed author", message, []model.Entry{entryOne}, map[string][]byte{entryOne.Path: one})
	if commitOne.Author != "sam" || commitOne.ActorID != memberUser.ID {
		t.Fatalf("commit trusted client author: %+v", commitOne)
	}
	if entries, err := client.Entries(ctx, commitOne.ID, model.Selection{{Path: "", Include: true}}); err != nil || len(entries) != 1 || entries[0] != entryOne {
		t.Fatalf("API entries %#v: %v", entries, err)
	}
	history, err := client.History(ctx, "main", 10)
	if err != nil || len(history) != 1 || history[0].ActorID != memberUser.ID {
		t.Fatalf("API history %#v: %v", history, err)
	}

	two := []byte("second binary version")
	entryTwo := fileEntry("<svg onload=alert(1)>.bin", two)
	commitTwo := saveFile(t, ctx, client, "main", commitOne.ID, "spoofed", "Second version", []model.Entry{{Path: entryOne.Path, Kind: model.Absent}, entryTwo}, map[string][]byte{entryTwo.Path: two})
	if err := client.CreateBranch(ctx, "work", commitTwo.ID); err != nil {
		t.Fatalf("writer could not create branch: %v", err)
	}
	deepOne := []byte("nested opaque file one\x00\xff")
	deepTwo := []byte("nested opaque file two\x01\xfe")
	deepPathOne := "assets folder/a b%é.bin"
	deepPathTwo := "folder?/deeper name/leaf?.bin"
	deepEntries := []model.Entry{
		{Path: "assets folder", Kind: model.Directory},
		fileEntry(deepPathOne, deepOne),
		{Path: "folder?", Kind: model.Directory},
		{Path: "folder?/deeper name", Kind: model.Directory},
		fileEntry(deepPathTwo, deepTwo),
	}
	deepCommit := saveFile(t, ctx, client, "work", commitTwo.ID, "spoofed", "Nested path version", deepEntries, map[string][]byte{deepPathOne: deepOne, deepPathTwo: deepTwo})
	if branches, err := client.Branches(ctx); err != nil || len(branches) != 2 {
		t.Fatalf("CLI branches %#v: %v", branches, err)
	}
	if page := member.get("/p/binary-artifacts/history"); page.status != http.StatusOK || !strings.Contains(page.body, "&lt;script&gt;") || strings.Contains(page.body, `<script>alert("unsafe")</script>`) {
		t.Fatalf("history message was not safely rendered: %d %s", page.status, page.body)
	}
	branchPage := member.get("/p/binary-artifacts/branches")
	if branchPage.status != http.StatusOK || !strings.Contains(branchPage.body, "work") {
		t.Fatalf("branch page omitted writer branch: %d %s", branchPage.status, branchPage.body)
	}
	filePage := member.get("/p/binary-artifacts")
	wantStatus(t, filePage, http.StatusOK)
	if !strings.Contains(filePage.body, "&lt;svg onload=alert(1)&gt;.bin") || strings.Contains(filePage.body, "<svg onload=alert(1)>") {
		t.Fatalf("file name was not escaped: %s", filePage.body)
	}

	// Follow the links rendered by the UI for opaque nested names. The branch
	// context must survive directory navigation, parent navigation, and history.
	branchFiles := member.get("/p/binary-artifacts?branch=work")
	wantStatus(t, branchFiles, http.StatusOK)
	if !selectedWorkBranch.MatchString(branchFiles.body) {
		t.Fatalf("work branch was not selected: %s", branchFiles.body)
	}
	folderHref := anchorWithText(t, branchFiles.body, "folder?")
	deeperPage := member.get(folderHref)
	wantStatus(t, deeperPage, http.StatusOK)
	if !selectedWorkBranch.MatchString(deeperPage.body) {
		t.Fatalf("folder navigation lost branch context: %s", deeperPage.body)
	}
	deepHref := anchorWithText(t, deeperPage.body, "deeper name")
	leafPage := member.get(deepHref)
	wantStatus(t, leafPage, http.StatusOK)
	if !selectedWorkBranch.MatchString(leafPage.body) {
		t.Fatalf("nested navigation lost branch context: %s", leafPage.body)
	}
	leafDownload := member.get(anchorWithText(t, leafPage.body, "Download ↓"))
	wantStatus(t, leafDownload, http.StatusOK)
	if !bytes.Equal([]byte(leafDownload.body), deepTwo) {
		t.Fatalf("deeply nested file download %q, want opaque bytes %q", leafDownload.body, deepTwo)
	}
	parentHref := anchorWithText(t, leafPage.body, "← Parent directory")
	parentPage := member.get(parentHref)
	wantStatus(t, parentPage, http.StatusOK)
	if !selectedWorkBranch.MatchString(parentPage.body) || !strings.Contains(parentPage.body, "deeper name") {
		t.Fatalf("parent navigation lost branch or directory: %s", parentPage.body)
	}
	assetHref := anchorWithText(t, branchFiles.body, "assets folder")
	assetPage := member.get(assetHref)
	wantStatus(t, assetPage, http.StatusOK)
	downloadHref := anchorWithText(t, assetPage.body, "Download ↓")
	download := member.get(downloadHref)
	wantStatus(t, download, http.StatusOK)
	if !bytes.Equal([]byte(download.body), deepOne) {
		t.Fatalf("nested file download %q, want opaque bytes %q", download.body, deepOne)
	}
	historyPage := member.get("/p/binary-artifacts/history?branch=work")
	wantStatus(t, historyPage, http.StatusOK)
	versionHref := anchorWithText(t, historyPage.body, "Nested path version")
	versionPage := member.get(versionHref)
	wantStatus(t, versionPage, http.StatusOK)
	versionURL, err := url.Parse(versionHref)
	if err != nil || versionURL.Query().Get("branch") != "work" || versionURL.Query().Get("version") != deepCommit.ID || !selectedWorkBranch.MatchString(versionPage.body) {
		t.Fatalf("history link lost version or branch context: href=%q page=%s err=%v", versionHref, versionPage.body, err)
	}

	// POST /entries and POST /read are read operations even though they carry
	// bodies. A writer-scoped token can read, save, but cannot manage retention.
	signature := new(bytes.Buffer)
	if err := transfer.Signature(ctx, bytes.NewReader(two), int64(len(two)), signature); err != nil {
		t.Fatal(err)
	}
	readURL := origin + "/p/binary-artifacts/v1/read?" + url.Values{"commit": {commitTwo.ID}, "path": {entryTwo.Path}}.Encode()
	readReq, err := http.NewRequestWithContext(ctx, http.MethodPost, readURL, signature)
	if err != nil {
		t.Fatal(err)
	}
	readReq.Header.Set("Authorization", "Bearer "+memberToken)
	readReq.Header.Set("Lode-Project-ID", firstProject.ID)
	readResp, err := http.DefaultClient.Do(readReq)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, readResp.Body)
	readResp.Body.Close()
	if readResp.StatusCode != http.StatusOK || readResp.Header.Get("Lode-Encoding") != "rsync" {
		t.Fatalf("POST /read classification status=%d encoding=%q", readResp.StatusCode, readResp.Header.Get("Lode-Encoding"))
	}

	adminClient, err := remote.New(origin+"/p/binary-artifacts", adminToken, firstProject.ID)
	if err != nil {
		t.Fatal(err)
	}
	planToken, err := adminClient.PreviewCleanup(ctx, 1, nil)
	if err != nil {
		t.Fatalf("maintainer cleanup preview: %v", err)
	}
	if count, err := adminClient.ApplyCleanup(ctx, 1, planToken); err != nil || count != 1 {
		t.Fatalf("cleanup expired %d versions: %v", count, err)
	}
	if page := member.get("/p/binary-artifacts?version=" + commitOne.ID); page.status != http.StatusOK || !strings.Contains(page.body, "Contents released") {
		t.Fatalf("released version metadata missing: %d %s", page.status, page.body)
	}
	expired := member.get("/p/binary-artifacts/download?" + url.Values{"version": {commitOne.ID}, "path": {entryOne.Path}}.Encode())
	wantStatus(t, expired, http.StatusGone)

	otherClient, err := remote.New(origin+"/p/private-notes", memberToken, secondProject.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherClient.Head(ctx, "main"); err == nil {
		t.Fatal("project-scoped token crossed into another project")
	}

	// A membership downgrade applies immediately to a previously issued token.
	wantStatus(t, admin.post("/p/binary-artifacts/members", postFields("username", "sam", "role", "reader")), http.StatusSeeOther)
	if _, err := client.Entries(ctx, commitTwo.ID, model.Selection{{Path: "", Include: true}}); err != nil {
		t.Fatalf("reader denied POST /entries: %v", err)
	}
	if err := client.CreateBranch(ctx, "should-fail", commitTwo.ID); err == nil {
		t.Fatal("reader created a branch")
	}
	if _, err := client.PreviewCleanup(ctx, 1, nil); err == nil {
		t.Fatal("reader previewed cleanup")
	}
	readerPage := member.get("/p/binary-artifacts/members")
	wantStatus(t, readerPage, http.StatusForbidden)
	wantStatus(t, member.get("/admin/users"), http.StatusForbidden)

	// The account cannot remove the final enabled administrator.
	adminUser, err := inst.LookupUser(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	lastAdmin := admin.post("/admin/users/state", postFields("id", adminUser.ID, "admin", "on", "disabled", "on"))
	wantStatus(t, lastAdmin, http.StatusBadRequest)
	wantStatus(t, admin.get("/admin/users"), http.StatusOK)

	// Password changes invalidate sibling browser sessions, while the active
	// session continues with the updated auth version.
	changedAgain := member.post("/settings/password", postFields("current", "sam's permanent passphrase", "password", "sam's next passphrase", "confirm", "sam's next passphrase"))
	wantStatus(t, changedAgain, http.StatusSeeOther)
	if page := sibling.get("/"); page.status != http.StatusSeeOther || page.header.Get("Location") != "/login" {
		t.Fatalf("old session survived password change: %d %q", page.status, page.header.Get("Location"))
	}
	wantStatus(t, member.get("/"), http.StatusOK)
	freshSibling := newBrowser(t, origin)
	wantStatus(t, freshSibling.post("/login", postFields("username", "sam", "password", "sam's next passphrase")), http.StatusSeeOther)

	// Seed a synthetic key so the real HTTP login takes the pending-MFA path;
	// recovery itself is exercised through the real one-use-code handler.
	security, err := instance.NewSecurity(inst.Store, origin)
	if err != nil {
		t.Fatal(err)
	}
	fakeID := []byte("synthetic-http-key")
	credentialData, err := json.Marshal(webauthn.Credential{ID: fakeID, PublicKey: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = inst.DB().ExecContext(ctx, `INSERT INTO security_keys(id,user_id,name,credential,created) VALUES(?,?,?,?,?)`, base64.RawURLEncoding.EncodeToString(fakeID), memberUser.ID, "synthetic key", credentialData, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	recoveryCodes, err := security.Recovery(ctx, memberUser.ID)
	if err != nil || len(recoveryCodes) != 10 {
		t.Fatalf("seed recovery codes: %d %v", len(recoveryCodes), err)
	}
	wantStatus(t, member.post("/logout", url.Values{}), http.StatusSeeOther)
	if page := freshSibling.get("/"); page.status != http.StatusSeeOther || page.header.Get("Location") != "/login" {
		t.Fatalf("recovery security change left sibling session active: %d %q", page.status, page.header.Get("Location"))
	}
	pendingLogin := member.post("/login", postFields("username", "sam", "password", "sam's next passphrase"))
	wantStatus(t, pendingLogin, http.StatusSeeOther)
	if pendingLogin.header.Get("Location") != "/verify" {
		t.Fatalf("MFA user not sent to pending verification: %q", pendingLogin.header.Get("Location"))
	}
	wantStatus(t, member.get("/verify"), http.StatusOK)
	if page := member.get("/p/binary-artifacts"); page.status != http.StatusSeeOther || page.header.Get("Location") != "/login" {
		t.Fatalf("pending MFA opened project page: %d %q", page.status, page.header.Get("Location"))
	}
	if page := member.post("/settings/tokens", postFields("name", "pending", "project", firstProject.ID, "role", "reader", "days", "90")); page.status != http.StatusSeeOther || page.header.Get("Location") != "/login" {
		t.Fatalf("pending MFA created token: %d %q", page.status, page.header.Get("Location"))
	}
	noBearer, err := http.Get(origin + "/p/binary-artifacts/v1/head?branch=main")
	if err != nil {
		t.Fatal(err)
	}
	noBearer.Body.Close()
	if noBearer.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session cookie authenticated API: %d", noBearer.StatusCode)
	}
	if bad := member.post("/verify/recovery", postFields("code", "not-a-recovery-code")); bad.status != http.StatusUnauthorized {
		t.Fatalf("invalid recovery code status %d", bad.status)
	}
	wantStatus(t, member.post("/verify/recovery", postFields("code", recoveryCodes[0])), http.StatusSeeOther)
	wantStatus(t, member.get("/"), http.StatusOK)
	wantStatus(t, member.post("/logout", url.Values{}), http.StatusSeeOther)
	pendingAgain := member.post("/login", postFields("username", "sam", "password", "sam's next passphrase"))
	wantStatus(t, pendingAgain, http.StatusSeeOther)
	if replay := member.post("/verify/recovery", postFields("code", recoveryCodes[0])); replay.status != http.StatusUnauthorized {
		t.Fatalf("replayed recovery code status %d", replay.status)
	}
	wantStatus(t, member.post("/verify/recovery", postFields("code", recoveryCodes[1])), http.StatusSeeOther)

	// Revocation and account disablement deny already issued credentials on
	// their next request, and unknown paths stay closed.
	wantStatus(t, member.post("/settings/tokens/revoke", postFields("id", tokens[0].ID)), http.StatusSeeOther)
	if _, err := client.Head(ctx, "main"); err == nil {
		t.Fatal("revoked token remained valid")
	}
	wantStatus(t, admin.get("/no-such-route"), http.StatusNotFound)
	wantStatus(t, admin.post("/admin/users/state", postFields("id", memberUser.ID, "disabled", "on")), http.StatusSeeOther)
	if page := member.get("/"); page.status != http.StatusSeeOther || page.header.Get("Location") != "/login" {
		t.Fatalf("disabled account retained browser access: %d %q", page.status, page.header.Get("Location"))
	}
	if _, err := client.Head(ctx, "main"); err == nil {
		t.Fatal("disabled account retained token access")
	}
}
