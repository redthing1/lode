package remote

import (
	"bytes"
	"context"
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
