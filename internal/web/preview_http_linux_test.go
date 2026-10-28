//go:build linux

package web

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/remote"
)

func TestProjectFileAndMediaPreviews(t *testing.T) {
	ctx := context.Background()
	inst, origin, setupCode := testServer(t)
	admin := newBrowser(t, origin)
	wantStatus(t, admin.post("/setup", postFields("code", setupCode, "username", "owner", "password", "a long enough password")), http.StatusSeeOther)
	wantStatus(t, admin.post("/projects/new", postFields("slug", "previews")), http.StatusSeeOther)
	info, err := inst.Project(ctx, "previews")
	if err != nil {
		t.Fatal(err)
	}
	tokenPage := admin.post("/settings/tokens", postFields("name", "preview-test", "project", info.ID, "role", "maintainer", "days", "90"))
	wantStatus(t, tokenPage, http.StatusOK)
	client, err := remote.New(origin+"/p/previews", secretFromPage(t, tokenPage.body), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	readme := []byte("# Preview Guide\n\n| Name | Value |\n| --- | --- |\n| safe | yes |\n\n[Source](source.html) ![Local](pixel.png) ![Escape](../../outside.png)\n")
	source := []byte("<script>alert(1)</script>\n<img src=x onerror=alert(2)>")
	pixel := []byte{0x89, 'P', 'N', 'G', 0, 1}
	wave := []byte("0123456789")
	archive := []byte{0x00, 0xff, 1, 2}
	changes := []model.Entry{
		{Path: "README.md", Kind: model.Directory}, fileEntry("README.md/child.txt", []byte("directory named README.md")),
		fileEntry("README", readme), fileEntry("source.html", source), fileEntry("pixel.png", pixel),
		fileEntry("sound.wav", wave), fileEntry("archive.bin", archive),
	}
	model.Sort(changes)
	first := saveFile(t, ctx, client, "main", "", "owner", "preview files", changes, map[string][]byte{
		"README.md/child.txt": []byte("directory named README.md"), "README": readme, "source.html": source, "pixel.png": pixel, "sound.wav": wave, "archive.bin": archive,
	})
	reader := newBrowser(t, origin)
	unauthorized := reader.get("/p/previews/file?" + url.Values{"version": {first.ID}, "path": {"README"}}.Encode())
	wantStatus(t, unauthorized, http.StatusSeeOther)
	if unauthorized.header.Get("Location") != "/login" {
		t.Fatalf("anonymous file preview redirected to %q, want login", unauthorized.header.Get("Location"))
	}

	page := admin.get("/p/previews?" + url.Values{"version": {first.ID}}.Encode())
	wantStatus(t, page, http.StatusOK)
	tableAt, readmeAt := strings.Index(page.body, "file-table"), strings.Index(page.body, "Preview Guide")
	if tableAt < 0 || readmeAt <= tableAt || !strings.Contains(page.body, "<table>") {
		t.Fatalf("root README was not rendered after its file table: %s", page.body)
	}
	if !strings.Contains(page.body, "&amp;version="+first.ID) || !strings.Contains(page.body, "/p/previews/raw?") || strings.Contains(page.body, "outside.png") {
		t.Fatalf("README links were not version-pinned and root-bounded: %s", page.body)
	}

	filePage := admin.get("/p/previews/file?" + url.Values{"version": {first.ID}, "path": {"source.html"}}.Encode())
	wantStatus(t, filePage, http.StatusOK)
	if strings.Contains(filePage.body, "<script>alert(1)</script>") || !strings.Contains(filePage.body, "&lt;script&gt;alert(1)&lt;/script&gt;") || !strings.Contains(filePage.body, "&lt;img src=x onerror=alert(2)&gt;") {
		t.Fatalf("HTML source was not shown as escaped text: %s", filePage.body)
	}

	mediaPath := "/p/previews/raw?" + url.Values{"version": {first.ID}, "path": {"sound.wav"}}.Encode()
	media := browserRequest(t, admin, mediaPath, "")
	if media.status != http.StatusOK || media.header.Get("Content-Type") != "audio/wav" || !strings.HasPrefix(media.header.Get("Content-Disposition"), "inline") || !bytes.Equal([]byte(media.body), wave) {
		t.Fatalf("wave response: status=%d headers=%v body=%q", media.status, media.header, media.body)
	}
	ranged := browserRequest(t, admin, mediaPath, "bytes=2-5")
	if ranged.status != http.StatusPartialContent || ranged.header.Get("Content-Range") != "bytes 2-5/10" || ranged.body != "2345" {
		t.Fatalf("range response: status=%d headers=%v body=%q", ranged.status, ranged.header, ranged.body)
	}
	unsatisfiable := browserRequest(t, admin, mediaPath, "bytes=10-")
	if unsatisfiable.status != http.StatusRequestedRangeNotSatisfiable || unsatisfiable.header.Get("Content-Range") != "bytes */10" {
		t.Fatalf("invalid range response: status=%d headers=%v", unsatisfiable.status, unsatisfiable.header)
	}
	image := browserRequest(t, admin, "/p/previews/raw?"+url.Values{"version": {first.ID}, "path": {"pixel.png"}}.Encode(), "")
	if image.status != http.StatusOK || image.header.Get("Content-Type") != "image/png" || !bytes.Equal([]byte(image.body), pixel) {
		t.Fatalf("image response: status=%d headers=%v body=%v", image.status, image.header, []byte(image.body))
	}

	binaryPage := admin.get("/p/previews/file?" + url.Values{"version": {first.ID}, "path": {"archive.bin"}}.Encode())
	wantStatus(t, binaryPage, http.StatusOK)
	if !strings.Contains(binaryPage.body, "Download file") || strings.Contains(binaryPage.header.Get("Location"), "/download") {
		t.Fatalf("binary file click did not show download fallback: %d %s", binaryPage.status, binaryPage.body)
	}

	// Advance history, release the older version, and verify both HTML and media
	// preview routes preserve the project's expiration boundary.
	saveFile(t, ctx, client, "main", first.ID, "owner", "advance", []model.Entry{fileEntry("README", []byte("# New version"))}, map[string][]byte{"README": []byte("# New version")})
	p, release, err := inst.Acquire(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := p.PreviewCleanup(ctx, 1)
	var expired int
	if err == nil {
		expired, err = p.ApplyCleanup(ctx, 1, plan.Token)
	}
	release()
	if err != nil || expired != 1 {
		t.Fatalf("cleanup expired %d versions: %v", expired, err)
	}
	for _, target := range []string{
		"/p/previews/file?" + url.Values{"version": {first.ID}, "path": {"README"}}.Encode(),
		"/p/previews/raw?" + url.Values{"version": {first.ID}, "path": {"sound.wav"}}.Encode(),
	} {
		response := admin.get(target)
		wantStatus(t, response, http.StatusGone)
	}
}

func browserRequest(t *testing.T, browser *browser, path, byteRange string) browserResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, browser.origin+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
	}
	resp, err := browser.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return browserResponse{status: resp.StatusCode, header: resp.Header.Clone(), body: string(body)}
}
