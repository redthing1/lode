package web

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
)

func TestFilePreviewsAndDownloads(t *testing.T) {
	ctx := context.Background()
	f := apiTest(t)
	b := newBrowserSession(t, f.origin)
	b.request("POST", "/login", url.Values{"username": {"alice"}, "password": {"a sufficiently long test password"}}, 303)
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 200, B: 70, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	data := map[string][]byte{
		"notes.txt":         []byte("exact text <script>not markup</script>"),
		"odd ?#&.bin":       []byte("opaque\x00\xff"),
		"images/pixel.png":  encoded.Bytes(),
		"images/odd ?#.PNG": encoded.Bytes(),
		"tone.wav":          []byte("RIFF\x00\x01binary WAVE contents"),
		"clip.webm":         []byte("\x1a\x45\xdf\xa3opaque WebM contents"),
		"large.mp4":         bytes.Repeat([]byte{0, 1, 2, 3}, (1<<18)+1),
		"vector.svg":        []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"page.html":         []byte(`<script>alert(1)</script>`),
		"docs/README.md":    []byte("# Images\n\n![pixel](../images/pixel.png)\n\n![odd](/images/odd%20%3F%23.PNG)\n\n[File](../images/pixel.png)\n\n![escape](../../escape.png)\n\n![remote](https://other.example/a.png)\n\n![protocol](//other.example/a.png)\n\n![active](../vector.svg)\n\n![query](../images/pixel.png?tracking=1)\n\n![fragment](../images/pixel.png#x)\n\n![data](data:image/png;base64,eA==)\n\n<img src=\"/unsafe.png\">\n"),
	}
	data["docs/README.md"] = append(data["docs/README.md"], []byte("\n<script>alert('x')</script>\n\n[Text](../notes.txt)\n")...)
	changes := []model.Entry{{Path: "images", Kind: model.Directory}, {Path: "docs", Kind: model.Directory}}
	for name, bytes := range data {
		changes = append(changes, entry(name, bytes))
	}
	v, err := f.client.Save(ctx, "alice", "project", model.Save{ID: operation(t), Branch: "main", Changes: changes}, nil, func(e model.Entry) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data[e.Path])), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	p := instance.Project{Owner: "alice", Name: "project"}
	request := func(method, path, span string, want int) (http.Header, []byte) {
		t.Helper()
		r, err := http.NewRequest(method, f.origin+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if span != "" {
			r.Header.Set("Range", span)
		}
		response, err := b.client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != want {
			t.Fatalf("%s %s range %q: %d expected %d; %v", method, path, span, response.StatusCode, want, err)
		}
		return response.Header, body
	}
	for _, tc := range []struct{ name, mime, element string }{
		{"images/pixel.png", "image/png", "<img "},
		{"images/odd ?#.PNG", "image/png", "<img "},
		{"tone.wav", "audio/wav", "<audio controls"},
		{"clip.webm", "video/webm", "<video controls"},
		{"large.mp4", "video/mp4", "<video controls"},
	} {
		path := mediaURL(p, v.ID, tc.name)
		header, body := request("GET", path, "", 200)
		if !bytes.Equal(body, data[tc.name]) || header.Get("Content-Type") != tc.mime || header.Get("Content-Disposition") != "inline" || header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("incorrect media response for %s", tc.name)
		}
		_, body = request("HEAD", path, "", 200)
		if len(body) != 0 {
			t.Fatal("HEAD returned bytes")
		}
		header, body = request("GET", path, "bytes=2-7", 206)
		if !bytes.Equal(body, data[tc.name][2:8]) || !strings.HasPrefix(header.Get("Content-Range"), "bytes 2-7/") {
			t.Fatal("range differs")
		}
		_, body = request("GET", path, "bytes=-4", 206)
		if !bytes.Equal(body, data[tc.name][len(data[tc.name])-4:]) {
			t.Fatal("suffix range differs")
		}
		request("GET", path, "bytes=99999999-", 416)
		request("GET", path, "bytes=0-1,4-5", 416)
		page := b.request("GET", treeURL(p, v.ID, tc.name, ""), nil, 200)
		if !strings.Contains(page, tc.element) || !strings.Contains(page, path) {
			t.Fatalf("missing %s preview", tc.name)
		}
	}
	for _, name := range []string{"vector.svg", "page.html", "docs/README.md"} {
		request("GET", mediaURL(p, v.ID, name), "", 415)
	}
	raw := "/alice/project/raw/" + v.ID + "/vector.svg"
	header, body := request("GET", raw, "", 200)
	if header.Get("Content-Type") != "application/octet-stream" || header.Get("Content-Disposition") != "attachment" || !bytes.Equal(body, data["vector.svg"]) {
		t.Fatal("download changed original bytes or disposition")
	}
	page := b.request("GET", treeURL(p, v.ID, "docs", ""), nil, 200)
	if !strings.Contains(page, "<h1>Images</h1>") || strings.Contains(page, "<script>alert") || !strings.Contains(page, "path=notes.txt") {
		t.Fatal("missing or unsafe Markdown preview")
	}
	if strings.Count(page, `<img src="/alice/project/media/`) != 2 || !strings.Contains(page, mediaURL(p, v.ID, "images/pixel.png")) || !strings.Contains(page, mediaURL(p, v.ID, "images/odd ?#.PNG")) {
		t.Fatal("local Markdown images missing or not version-bound")
	}
	for _, forbidden := range []string{"other.example", "escape.png", "unsafe.png", "tracking=1", "data:image", "/media/" + v.ID + "/vector.svg"} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("unsafe Markdown destination: %s", forbidden)
		}
	}
	if !strings.Contains(page, "path=images%2Fpixel.png") || !strings.Contains(page, "version="+v.ID) {
		t.Fatal("relative file link lost immutable target")
	}
	page = b.request("GET", treeURL(p, v.ID, "notes.txt", ""), nil, 200)
	if !strings.Contains(page, "&lt;script&gt;not markup&lt;/script&gt;") {
		t.Fatal("text preview was not escaped")
	}
	u := url.URL{Path: "/alice/project/raw/" + v.ID + "/odd ?#&.bin"}
	_, body = request("GET", u.String(), "", 200)
	if !bytes.Equal(body, data["odd ?#&.bin"]) {
		t.Fatal("download bytes differ")
	}
	updated, err := f.client.Save(ctx, "alice", "project", model.Save{ID: operation(t), Base: v.ID, Branch: "main", Changes: []model.Entry{entry("images/pixel.png", []byte("replacement"))}}, nil, func(model.Entry) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("replacement")), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, body = request("GET", mediaURL(p, v.ID, "images/pixel.png"), "", 200)
	if !bytes.Equal(body, data["images/pixel.png"]) {
		t.Fatal("old media followed branch change")
	}
	err = f.store.WithProject(ctx, instance.Principal{User: f.user}, "alice", "project", instance.Maintainer, func(_ instance.Project, store *project.Store) error {
		if err := store.SetRetention(ctx, model.Retention{Versions: 1, HistoryDays: 90}); err != nil {
			return err
		}
		preview, err := store.PreviewCleanup(ctx)
		if err != nil {
			return err
		}
		_, err = store.Cleanup(ctx, preview.Token)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	request("GET", mediaURL(p, v.ID, "images/pixel.png"), "", 410)
	request("HEAD", mediaURL(p, v.ID, "images/pixel.png"), "", 410)
	page = b.request("GET", treeURL(p, v.ID, "images/pixel.png", ""), nil, 200)
	if strings.Contains(page, `<img src="/alice/project/media/`) || !strings.Contains(page, "contents have expired") {
		t.Fatal("expired preview still offered")
	}
	b.request("POST", "/admin/users/new", url.Values{"username": {"bob"}, "password": {"a sufficiently long test password"}}, 303)
	bob := newBrowserSession(t, f.origin)
	bob.request("POST", "/login", url.Values{"username": {"bob"}, "password": {"a sufficiently long test password"}}, 303)
	bob.request("GET", mediaURL(p, updated.ID, "images/pixel.png"), nil, 403)
	anonymous := newBrowserSession(t, f.origin)
	anonymous.request("GET", mediaURL(p, updated.ID, "images/pixel.png"), nil, 303)
	request("GET", mediaURL(p, updated.ID, "images/pixel.png"), "", 200)
}
