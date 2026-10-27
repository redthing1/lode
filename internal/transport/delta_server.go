package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/transfer"
)

const (
	ChangesPreparePath   = "/v1/changes/prepare"
	ChangesSignaturePath = "/v1/changes/signature"
	DeltaMediaType       = "application/vnd.lode.rsync-delta-v1"
)

func (s *server) servePrepareChanges(w http.ResponseWriter, r *http.Request) {
	var request project.ChangeRequest
	if !decodeJSONBody(w, r, &request) {
		return
	}
	operation, err := s.service.PrepareChanges(r.Context(), request)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, operation)
}

func (s *server) serveChangeSignature(w http.ResponseWriter, r *http.Request) {
	query, ok := readQuery(w, r, "operation", "path")
	if !ok {
		return
	}
	signature, err := s.service.Signature(r.Context(), query.Get("operation"), query.Get("path"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, signature)
}

func uploadMediaType(part *multipart.Part) (string, error) {
	value := strings.TrimSpace(part.Header.Get("Content-Type"))
	if value == "" {
		return "", nil
	}
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil || len(parameters) != 0 {
		return "", fmt.Errorf("%w: invalid file part content type", errInvalidUpload)
	}
	return mediaType, nil
}

// deltaUploadPart counts reads from one encoded multipart part. It never
// advances to a later part unless the decoder has observed the exact end of
// this part after validating the delta terminator.
type deltaUploadPart struct {
	*multipart.Part
	last     bool
	nextPart func() (*multipart.Part, error)
	eof      bool
	closed   bool
}

func (p *deltaUploadPart) Read(dst []byte) (int, error) {
	if p.closed {
		return 0, io.EOF
	}
	n, err := p.Part.Read(dst)
	if errors.Is(err, io.EOF) {
		p.eof = true
	}
	return n, err
}

func (p *deltaUploadPart) finishVerified() error {
	if p.closed {
		return errors.New("delta upload part already closed")
	}
	if !p.eof {
		return fmt.Errorf("%w: delta decoder did not verify part EOF", errInvalidUpload)
	}
	p.closed = true
	if err := p.Part.Close(); err != nil {
		return fmt.Errorf("%w: close delta part: %v", errInvalidUpload, err)
	}
	if p.last {
		extra, err := p.nextPart()
		if extra != nil {
			return fmt.Errorf("%w: unexpected trailing multipart part", errInvalidUpload)
		}
		if err != io.EOF {
			return fmt.Errorf("%w: check multipart trailer", errInvalidUpload)
		}
	}
	return nil
}

// deltaTargetPart exposes only reconstructed target bytes to the engine. A
// malformed decoder result is terminal: Close does not drain the remaining
// untrusted multipart payload.
type deltaTargetPart struct {
	decoder  io.Reader
	encoded  *deltaUploadPart
	size     int64
	sha256   string
	hasher   hash.Hash
	read     int64
	verified bool
	terminal error
}

func newDeltaUploadPart(ctx context.Context, basis io.ReadSeeker, basisSize, targetSize int64, digest string, encoded *deltaUploadPart) (*deltaTargetPart, error) {
	decoder, err := transfer.NewReader(ctx, basis, basisSize, targetSize, encoded)
	if err != nil {
		return nil, err
	}
	return &deltaTargetPart{decoder: decoder, encoded: encoded, size: targetSize, sha256: digest, hasher: sha256.New()}, nil
}

func (p *deltaTargetPart) Read(dst []byte) (int, error) {
	if p.terminal != nil {
		return 0, p.terminal
	}
	if p.verified {
		return 0, io.EOF
	}
	n, err := p.decoder.Read(dst)
	if n > 0 {
		p.read += int64(n)
		_, _ = p.hasher.Write(dst[:n])
		if p.read > p.size {
			p.terminal = fmt.Errorf("%w: reconstructed delta exceeds target size", errInvalidUpload)
			return n, p.terminal
		}
	}
	if err != nil && err != io.EOF {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			p.terminal = err
		} else {
			p.terminal = errors.Join(errInvalidUpload, err)
		}
		return n, p.terminal
	}
	if err == io.EOF {
		if p.read != p.size {
			p.terminal = fmt.Errorf("%w: reconstructed length is %d, expected %d", errInvalidUpload, p.read, p.size)
			return n, p.terminal
		}
		if hex.EncodeToString(p.hasher.Sum(nil)) != p.sha256 {
			p.terminal = fmt.Errorf("%w: reconstructed SHA256 differs from manifest", errInvalidUpload)
			return n, p.terminal
		}
		if finishErr := p.encoded.finishVerified(); finishErr != nil {
			p.terminal = finishErr
			return n, p.terminal
		}
		p.verified = true
	}
	return n, err
}

func (p *deltaTargetPart) Close() error {
	if p.terminal != nil {
		return p.terminal
	}
	if !p.verified {
		return fmt.Errorf("%w: delta target was not fully verified", errInvalidUpload)
	}
	return nil
}

var _ io.ReadCloser = (*deltaTargetPart)(nil)
var _ io.Reader = (*deltaUploadPart)(nil)
