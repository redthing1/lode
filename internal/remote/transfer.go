package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/project"
	"github.com/redthing1/lode/internal/transfer"
)

const minDeltaSize = 1 << 20
const signatureBudget = 64 << 20

// read holds the normal project read protection for either representation.
// POST supplies a bounded client signature; GET requests the complete file.
func (h *handler) read(w http.ResponseWriter, r *http.Request) {
	var sig []byte
	if r.Method == http.MethodPost {
		var err error
		sig, err = io.ReadAll(http.MaxBytesReader(w, r.Body, transfer.MaxSignatureSize))
		if err != nil || len(sig) == 0 {
			h.fail(w, project.ErrInvalid)
			return
		}
	}
	out := &downloadResponse{ResponseWriter: w}
	err := h.p.Read(r.Context(), r.URL.Query().Get("commit"), r.URL.Query().Get("path"), func(e model.Entry, rd io.Reader) error {
		w.Header().Set("Content-Type", "application/octet-stream")
		if sig != nil {
			w.Header().Set("Lode-Encoding", "rsync")
			if err := transfer.Delta(r.Context(), bytes.NewReader(sig), rd, out); err != nil {
				return fmt.Errorf("%w: delta download: %v", project.ErrInvalid, err)
			}
			return nil
		}
		w.Header().Set("Content-Length", fmt.Sprint(e.Size))
		_, err := io.Copy(out, rd)
		return err
	})
	if err != nil {
		if out.started {
			panic(http.ErrAbortHandler)
		}
		w.Header().Del("Lode-Encoding")
		w.Header().Del("Content-Length")
		h.fail(w, err)
	}
}

type downloadResponse struct {
	http.ResponseWriter
	started bool
}

func (w *downloadResponse) Write(p []byte) (int, error) {
	w.started = true
	return w.ResponseWriter.Write(p)
}

// Download reconstructs and verifies one exact target. A basis is optional and
// remains owned by the caller. No partial output may be installed on error.
func (c *Client) Download(ctx context.Context, id string, target model.Entry, basis io.ReadSeeker, basisSize int64, dst io.Writer) error {
	if err := target.Validate(); err != nil {
		return err
	}
	if target.Kind != model.File || target.Size == math.MaxInt64 {
		return project.ErrInvalid
	}
	var body io.ReadCloser
	useDelta := basis != nil && target.Size >= minDeltaSize && basisSize >= minDeltaSize && basisSize <= transfer.MaxBasisSize
	if useDelta {
		if _, err := basis.Seek(0, io.SeekStart); err != nil {
			return err
		}
		var sig bytes.Buffer
		if err := transfer.Signature(ctx, basis, basisSize, &sig); err != nil {
			return err
		}
		query := url.Values{"commit": {id}, "path": {target.Path}}
		resp, err := c.request(ctx, http.MethodPost, "read?"+query.Encode(), &sig, "application/octet-stream")
		if err != nil {
			return err
		}
		body = resp.Body
		defer body.Close()
		if err := c.verify(resp); err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return parseError(resp)
		}
		if resp.Header.Get("Lode-Encoding") != "rsync" {
			return errors.New("unexpected download encoding")
		}
	} else {
		var err error
		body, err = c.Read(ctx, id, target.Path)
		if err != nil {
			return err
		}
		defer body.Close()
	}
	hash := sha256.New()
	out := &downloadWriter{out: io.MultiWriter(dst, hash), remaining: target.Size}
	if useDelta {
		limited := &io.LimitedReader{R: body, N: deltaLimit(target.Size)}
		if err := transfer.Patch(ctx, basis, basisSize, limited, target.Size, out); err != nil {
			return err
		}
		if limited.N == 0 {
			return errors.New("download delta exceeds encoded size limit")
		}
	} else if _, err := io.Copy(out, io.LimitReader(body, target.Size+1)); err != nil {
		return err
	}
	if out.remaining != 0 || hex.EncodeToString(hash.Sum(nil)) != target.SHA256 {
		return fmt.Errorf("download identity mismatch for %s", target.Path)
	}
	return nil
}

type downloadWriter struct {
	out       io.Writer
	remaining int64
}

func (w *downloadWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("download exceeds declared size")
	}
	n, err := w.out.Write(p)
	w.remaining -= int64(n)
	return n, err
}

func deltaLimit(size int64) int64 {
	if size > (math.MaxInt64-(64<<10))/2 {
		return math.MaxInt64
	}
	return 2*size + (64 << 10)
}

func (h *handler) signature(w http.ResponseWriter, r *http.Request) {
	var sig bytes.Buffer
	err := h.p.Read(r.Context(), r.URL.Query().Get("commit"), r.URL.Query().Get("path"), func(e model.Entry, rd io.Reader) error {
		if e.Size < minDeltaSize || e.Size > transfer.MaxBasisSize {
			return nil
		}
		return transfer.Signature(r.Context(), rd, e.Size, &sig)
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	if sig.Len() == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(sig.Len()))
	_, _ = w.Write(sig.Bytes())
}

// Signatures are fetched before Save holds the exclusive project gate. One
// anonymous temporary file bounds RAM without adding persistent recovery state.
// Above the per-save spool budget, remaining files use the full representation.
type saveSignatures struct {
	file   *os.File
	ranges map[string][2]int64
}

func (s *saveSignatures) close() {
	if s.file != nil {
		_ = s.file.Close()
	}
}

func (s *saveSignatures) reader(name string) io.Reader {
	span, ok := s.ranges[name]
	if !ok {
		return nil
	}
	return io.NewSectionReader(s.file, span[0], span[1])
}

func (c *Client) saveSignatures(ctx context.Context, req project.Request) (_ *saveSignatures, err error) {
	s := &saveSignatures{ranges: make(map[string][2]int64)}
	defer func() {
		if err != nil {
			s.close()
		}
	}()
	if req.Base == "" {
		return s, nil
	}
	var used int64
	for _, e := range req.Changes {
		if e.Kind != model.File || e.Size < minDeltaSize || used > signatureBudget-transfer.MaxSignatureSize {
			continue
		}
		query := url.Values{"commit": {req.Base}, "path": {e.Path}}
		resp, err := c.request(ctx, http.MethodGet, "signature?"+query.Encode(), nil, "")
		if err != nil {
			return nil, err
		}
		data, readErr := func() ([]byte, error) {
			defer resp.Body.Close()
			if err := c.verify(resp); err != nil {
				return nil, err
			}
			if resp.StatusCode == http.StatusNoContent {
				return nil, nil
			}
			if resp.StatusCode != http.StatusOK {
				err := parseError(resp)
				if errors.Is(err, project.ErrNotFound) || errors.Is(err, project.ErrExpired) {
					return nil, nil
				}
				return nil, err
			}
			data, err := io.ReadAll(io.LimitReader(resp.Body, transfer.MaxSignatureSize+1))
			if len(data) > transfer.MaxSignatureSize {
				return nil, errors.New("remote signature exceeds limit")
			}
			return data, err
		}()
		if readErr != nil {
			return nil, readErr
		}
		if len(data) == 0 {
			continue
		}
		if s.file == nil {
			s.file, err = os.CreateTemp("", "lode-signatures-*")
			if err != nil {
				return nil, err
			}
			if err := os.Remove(s.file.Name()); err != nil {
				return nil, err
			}
		}
		if _, err := s.file.Write(data); err != nil {
			return nil, err
		}
		s.ranges[e.Path] = [2]int64{used, int64(len(data))}
		used += int64(len(data))
	}
	return s, nil
}
