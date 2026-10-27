package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/transfer"
)

func newHTTP2Fixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir() + "/project"
	if err := project.InitializeLocal(ctx, dir); err != nil {
		t.Fatal(err)
	}
	local, err := project.OpenLocal(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(local.Service, local.Token)
	if err != nil {
		_ = local.Close()
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(handler)
	ts.EnableHTTP2 = true
	ts.StartTLS()
	u, err := url.Parse(ts.URL)
	if err != nil {
		ts.Close()
		_ = local.Close()
		t.Fatal(err)
	}
	client := &Client{base: u, token: local.Token, http: ts.Client()}
	f := &fixture{local: local, server: ts, client: client}
	t.Cleanup(func() {
		client.Close()
		ts.Close()
		_ = local.Close()
	})
	return f
}

func TestDeltaMultipartOverHTTP2(t *testing.T) {
	f := newHTTP2Fixture(t)
	ctx := context.Background()
	basis := patternedBytes(2 << 20)
	base := createTransportBase(t, f, basis)
	target := append([]byte(nil), basis...)
	target[1_100_000] ^= 0x42
	request := deltaRequest("delta-http2", base.ID, target)
	prepareChangesHTTP(t, f, request)
	signature := requestSignatureHTTP(t, f, request.OperationID, "asset.bin")
	var encoded bytes.Buffer
	if err := transfer.Delta(ctx, signature.Data, int64(len(basis)), int64(len(target)), bytes.NewReader(target), &encoded); err != nil {
		t.Fatal(err)
	}
	response := postDeltaHTTP(t, f, request, encoded.Bytes(), DeltaMediaType, false)
	defer response.Body.Close()
	if response.ProtoMajor != 2 {
		t.Fatalf("delta HTTP protocol = %s, want HTTP/2", response.Proto)
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		t.Fatalf("delta HTTP/2 status=%d body=%s", response.StatusCode, body)
	}
	var commit project.Commit
	if err := decodeResponse(response, &commit); err != nil {
		t.Fatal(err)
	}
	reader, _, err := f.local.Service.OpenFile(ctx, commit.ID, "asset.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, target) {
		t.Fatalf("HTTP/2 reconstructed bytes differ: read=%v close=%v", readErr, closeErr)
	}
}

func TestMalformedDeltaResponseDoesNotDrainRemainingPart(t *testing.T) {
	f := newFixture(t)
	basis := patternedBytes(2 << 20)
	base := createTransportBase(t, f, basis)
	target := append([]byte(nil), basis...)
	target[123_456] ^= 1
	manifest, err := json.Marshal(deltaRequest("delta-no-drain", base.ID, target))
	if err != nil {
		t.Fatal(err)
	}

	bodyReader, bodyWriter := io.Pipe()
	defer bodyWriter.CloseWithError(errors.New("stop incomplete multipart body"))
	form := multipart.NewWriter(bodyWriter)
	request, err := http.NewRequest(http.MethodPost, f.server.URL+"/v1/changes", bodyReader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+f.local.Token)
	request.Header.Set("Content-Type", form.FormDataContentType())
	type responseResult struct {
		response *http.Response
		err      error
	}
	result := make(chan responseResult, 1)
	go func() {
		response, err := f.server.Client().Do(request)
		result <- responseResult{response: response, err: err}
	}()

	manifestPart, err := form.CreateFormField("manifest")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manifestPart.Write(manifest); err != nil {
		t.Fatal(err)
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file:0"; filename="data"`)
	header.Set("Content-Type", DeltaMediaType)
	part, err := form.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte{0, 0, 0, 0}); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write malformed delta header: %v", err)
	}

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatal(got.err)
		}
		defer got.response.Body.Close()
		if got.response.StatusCode != http.StatusBadRequest {
			t.Fatalf("malformed delta status=%d, want=400", got.response.StatusCode)
		}
		operation, err := f.local.Service.Operation(context.Background(), "delta-no-drain")
		if err != nil || operation.State != project.OperationCapturing {
			t.Fatalf("operation after malformed delta=%+v err=%v; want retryable capture", operation, err)
		}
		head, err := f.local.Service.Head(context.Background(), "main")
		if err != nil || head != base.ID {
			t.Fatalf("head after malformed delta=%q err=%v; want %q", head, err, base.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not reject malformed delta until unread multipart remainder was closed")
	}
}

func TestPrepareSignatureAndDeltaMultipart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	basis := patternedBytes(2 << 20)
	base := createTransportBase(t, f, basis)
	target := append([]byte(nil), basis...)
	copy(target[700_000:700_041], []byte("small edited run across one block boundary"))
	request := deltaRequest("delta-success", base.ID, target)

	operation := prepareChangesHTTP(t, f, request)
	if operation.State != project.OperationCapturing || operation.ID != request.OperationID || operation.Base != base.ID {
		t.Fatalf("prepare operation = %+v", operation)
	}
	signature := requestSignatureHTTP(t, f, request.OperationID, "asset.bin")
	if signature.OperationID != request.OperationID || signature.Base != base.ID || signature.Path != "asset.bin" || signature.Basis.Kind != model.File || signature.Basis.Size != int64(len(basis)) || len(signature.Data) == 0 {
		t.Fatalf("signature response = %+v, bytes=%d", signature, len(signature.Data))
	}

	var encoded bytes.Buffer
	if err := transfer.Delta(ctx, signature.Data, int64(len(basis)), int64(len(target)), bytes.NewReader(target), &encoded); err != nil {
		t.Fatalf("Delta() error = %v", err)
	}
	if encoded.Len() >= len(target) {
		t.Fatalf("test delta is not smaller than target: delta=%d target=%d", encoded.Len(), len(target))
	}
	commit := uploadDeltaHTTP(t, f, request, encoded.Bytes(), DeltaMediaType, false)
	if commit.ID == "" || len(commit.Parents) != 1 || commit.Parents[0] != base.ID {
		t.Fatalf("delta commit = %+v", commit)
	}
	reader, node, err := f.local.Service.OpenFile(ctx, commit.ID, "asset.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, target) || node.SHA256 != digestBytes(target) {
		t.Fatalf("stored target length=%d, node=%+v, read=%v, close=%v", len(got), node, readErr, closeErr)
	}
	operation, err = f.local.Service.Operation(ctx, request.OperationID)
	if err != nil || operation.State != project.OperationAccepted || operation.CommitID != commit.ID {
		t.Fatalf("operation after delta = %+v, %v", operation, err)
	}
}

func TestTransferRoutesRejectBadRequestsAndAuthentication(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, f.server.URL+ChangesPreparePath, strings.NewReader(`{"unknown":true}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+f.local.Token)
	request.Header.Set("Content-Type", "application/json")
	response, err := f.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("prepare unknown-field status = %d, want 400", response.StatusCode)
	}

	response, err = f.server.Client().Get(f.server.URL + ChangesSignaturePath + "?operation=missing&path=x")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated signature status = %d, want 401", response.StatusCode)
	}

	query := url.Values{"operation": {"missing"}, "path": {"x"}, "extra": {"bad"}}
	response, err = f.client.do(ctx, http.MethodGet, ChangesSignaturePath, query, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("signature extra-query status = %d, want 400", response.StatusCode)
	}
	response, err = f.client.do(ctx, http.MethodGet, ChangesSignaturePath, url.Values{"operation": {"missing"}}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("signature missing-query status = %d, want 400", response.StatusCode)
	}
}

func TestDeltaMultipartRejectsMalformedAndUnverifiedParts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	basis := patternedBytes(2 << 20)
	base := createTransportBase(t, f, basis)
	target := append([]byte(nil), basis...)
	target[900_000] ^= 0x53
	type malformedCase struct {
		name       string
		prepare    func(project.ChangeRequest) project.ChangeRequest
		mutate     func([]byte) []byte
		mediaType  string
		extraPart  bool
		wantStatus int
	}
	cases := []malformedCase{
		{name: "truncated", mutate: func(data []byte) []byte { return data[:len(data)-1] }, mediaType: DeltaMediaType, wantStatus: http.StatusBadRequest},
		{name: "trailing delta bytes", mutate: func(data []byte) []byte { return append(append([]byte(nil), data...), 0xff) }, mediaType: DeltaMediaType, wantStatus: http.StatusBadRequest},
		{name: "wrong reconstructed digest", prepare: func(req project.ChangeRequest) project.ChangeRequest {
			req.Changes[0].SHA256 = strings.Repeat("0", 64)
			return req
		}, mediaType: DeltaMediaType, wantStatus: http.StatusBadRequest},
		{name: "extra multipart part", extraPart: true, mediaType: DeltaMediaType, wantStatus: http.StatusBadRequest},
		{name: "unknown encoding", mediaType: "application/x-unknown", wantStatus: http.StatusBadRequest},
	}

	for i, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := deltaRequest(fmt.Sprintf("delta-invalid-%d", i), base.ID, target)
			if test.prepare != nil {
				request = test.prepare(request)
			}
			operation := prepareChangesHTTP(t, f, request)
			if operation.State != project.OperationCapturing {
				t.Fatalf("prepared operation state=%s", operation.State)
			}
			signature := requestSignatureHTTP(t, f, request.OperationID, "asset.bin")
			var encoded bytes.Buffer
			if err := transfer.Delta(ctx, signature.Data, int64(len(basis)), int64(len(target)), bytes.NewReader(target), &encoded); err != nil {
				t.Fatal(err)
			}
			data := encoded.Bytes()
			if test.mutate != nil {
				data = test.mutate(data)
			}
			response := postDeltaHTTP(t, f, request, data, test.mediaType, test.extraPart)
			defer response.Body.Close()
			if response.StatusCode != test.wantStatus {
				body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
				t.Fatalf("upload status=%d want=%d body=%s", response.StatusCode, test.wantStatus, body)
			}
			op, err := f.local.Service.Operation(ctx, request.OperationID)
			if err != nil || op.State != project.OperationCapturing {
				t.Fatalf("failed transfer operation=%+v err=%v; want retryable capture", op, err)
			}
			head, err := f.local.Service.Head(ctx, "main")
			if err != nil || head != base.ID {
				t.Fatalf("head after rejected transfer=%q err=%v want=%q", head, err, base.ID)
			}
		})
	}
}

func TestDeltaMultipartRejectsCommandLimitWithoutAccepting(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	basis := patternedBytes(2 << 20)
	base := createTransportBase(t, f, basis)
	const commandLimit = 1 << 20
	target := bytes.Repeat([]byte{'x'}, commandLimit+1)
	request := deltaRequest("delta-command-limit", base.ID, target)
	prepareChangesHTTP(t, f, request)
	requestSignatureHTTP(t, f, request.OperationID, "asset.bin")

	var encoded bytes.Buffer
	encoded.Write([]byte{0x72, 0x73, 0x02, 0x36})
	for i := 0; i <= commandLimit; i++ {
		encoded.Write([]byte{1, 'x'})
	}
	encoded.WriteByte(0)
	response := postDeltaHTTP(t, f, request, encoded.Bytes(), DeltaMediaType, false)
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		t.Fatalf("command-limit status=%d want=400 body=%s", response.StatusCode, body)
	}
	op, err := f.local.Service.Operation(ctx, request.OperationID)
	if err != nil || op.State != project.OperationCapturing {
		t.Fatalf("command-limit operation=%+v err=%v; want retryable capture", op, err)
	}
}

func createTransportBase(t *testing.T, f *fixture, contents []byte) project.Commit {
	t.Helper()
	request := project.ChangeRequest{
		OperationID: "delta-base", Branch: "main", Author: "transport test", Message: "base",
		Changes: []project.Change{changeBytes("asset.bin", contents)},
	}
	commit, err := f.local.Service.Apply(context.Background(), request, func(model.Path) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(contents)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return commit
}

func deltaRequest(operationID, baseID string, contents []byte) project.ChangeRequest {
	return project.ChangeRequest{
		OperationID: operationID, Branch: "main", Base: baseID, Author: "transport test", Message: "delta update",
		Changes: []project.Change{changeBytes("asset.bin", contents)},
	}
}

func changeBytes(path string, contents []byte) project.Change {
	return project.Change{Path: path, Kind: model.File, SHA256: digestBytes(contents), Size: int64(len(contents))}
}

func digestBytes(contents []byte) string {
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}

func patternedBytes(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte((i*31 + i/997) % 251)
	}
	return data
}

func prepareChangesHTTP(t *testing.T, f *fixture, request project.ChangeRequest) project.Operation {
	t.Helper()
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	response, err := f.client.do(context.Background(), http.MethodPost, ChangesPreparePath, nil, io.NopCloser(bytes.NewReader(data)), "application/json")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("prepare status=%d: %v", response.StatusCode, decodeWireError(response))
	}
	var operation project.Operation
	if err := decodeResponse(response, &operation); err != nil {
		t.Fatal(err)
	}
	return operation
}

func requestSignatureHTTP(t *testing.T, f *fixture, operationID, path string) project.FileSignature {
	t.Helper()
	var result project.FileSignature
	err := f.client.getJSON(context.Background(), ChangesSignaturePath, url.Values{"operation": {operationID}, "path": {path}}, &result)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func uploadDeltaHTTP(t *testing.T, f *fixture, request project.ChangeRequest, encoded []byte, mediaType string, extraPart bool) project.Commit {
	t.Helper()
	response := postDeltaHTTP(t, f, request, encoded, mediaType, extraPart)
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		t.Fatalf("delta upload status=%d body=%s", response.StatusCode, body)
	}
	var commit project.Commit
	if err := decodeResponse(response, &commit); err != nil {
		t.Fatal(err)
	}
	return commit
}

func postDeltaHTTP(t *testing.T, f *fixture, request project.ChangeRequest, encoded []byte, mediaType string, extraPart bool) *http.Response {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	manifest, err := writer.CreateFormField("manifest")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(manifest).Encode(request); err != nil {
		t.Fatal(err)
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file:0"; filename="data"`)
	if mediaType != "" {
		header.Set("Content-Type", mediaType)
	}
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(encoded); err != nil {
		t.Fatal(err)
	}
	if extraPart {
		if err := writer.WriteField("trailing", "unexpected"); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	requestHTTP, err := http.NewRequest(http.MethodPost, f.server.URL+"/v1/changes", &body)
	if err != nil {
		t.Fatal(err)
	}
	requestHTTP.Header.Set("Authorization", "Bearer "+f.local.Token)
	requestHTTP.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := f.server.Client().Do(requestHTTP)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
