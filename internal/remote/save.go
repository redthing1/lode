package remote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/redthing1/lode/internal/model"
	"github.com/redthing1/lode/internal/progress"
	"github.com/redthing1/lode/internal/transfer"
)

func (c *Client) Resolve(ctx context.Context, owner, name string, request model.Save) (model.Version, error) {
	var version model.Version
	err := c.json(ctx, "POST", projectPath(owner, name)+"/resolve", request, &version)
	return version, err
}

func (c *Client) Cancel(ctx context.Context, owner, name string, request model.Save) (model.Version, error) {
	var version model.Version
	err := c.json(ctx, "POST", projectPath(owner, name)+"/cancel", request, &version)
	return version, err
}

type signatureRange struct{ offset, size int64 }

// Save resolves accepted retries before opening local files. Signatures are
// prepared before publication takes the mutation lock and spooled to one temp file.
func (c *Client) Save(ctx context.Context, owner, name string, request model.Save, baseline []model.Entry, open func(model.Entry) (io.ReadCloser, error)) (model.Version, error) {
	progress.Report(ctx, progress.Event{Phase: "Checking save"})
	previous, err := c.Resolve(ctx, owner, name, request)
	if err == nil {
		return previous, nil
	}
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "unaccepted" {
		return model.Version{}, err
	}
	if err := request.Validate(); err != nil {
		return model.Version{}, err
	}
	metadata, err := json.Marshal(request)
	if err != nil {
		return model.Version{}, err
	}
	if len(metadata) > MaxMetadata {
		return model.Version{}, errors.New("request metadata exceeds limit")
	}
	bases := map[string]model.Entry{}
	for _, e := range baseline {
		bases[e.Path] = e
	}
	var files []model.Entry
	for _, e := range request.Changes {
		if e.Kind == model.File {
			files = append(files, e)
		}
	}
	// File parts follow the protocol's depth-first, sorted-component order.
	sort.Slice(files, func(i, j int) bool {
		return strings.ReplaceAll(files[i].Path, "/", "\x00") < strings.ReplaceAll(files[j].Path, "/", "\x00")
	})
	paths := make([]string, len(files))
	for i, e := range files {
		paths[i] = e.Path
	}
	ctx = progress.WithFiles(ctx, paths)
	var spool *os.File
	defer func() {
		if spool != nil {
			spool.Close()
			os.Remove(spool.Name())
		}
	}()
	signatures := map[string]signatureRange{}
	for _, e := range files {
		base := bases[e.Path]
		if request.Base == "" || base.Kind != model.File || base.Size < 1<<20 || base.Size > transfer.MaxBasisSize || e.Size < 1<<20 {
			continue
		}
		if spool == nil {
			spool, err = os.CreateTemp("", "lode-signatures-*")
			if err != nil {
				return model.Version{}, err
			}
		}
		progress.Report(ctx, progress.Event{Phase: "Preparing transfer", Path: e.Path})
		r, err := c.request(ctx, "GET", versionPath(owner, name, request.Base)+"/signature?path="+url.QueryEscape(e.Path), nil, "")
		if err != nil {
			return model.Version{}, err
		}
		offset, err := spool.Seek(0, io.SeekCurrent)
		if err != nil {
			r.Body.Close()
			return model.Version{}, err
		}
		n, err := io.Copy(spool, io.LimitReader(r.Body, transfer.MaxSignatureSize+1))
		r.Body.Close()
		if err != nil {
			return model.Version{}, err
		}
		if n > transfer.MaxSignatureSize {
			return model.Version{}, errors.New("signature exceeds limit")
		}
		signatures[e.Path] = signatureRange{offset, n}
	}
	reader, writer := io.Pipe()
	body := multipart.NewWriter(writer)
	done := make(chan error, 1)
	go func() {
		err := func() error {
			part, err := body.CreateFormField("request")
			if err != nil {
				return err
			}
			if _, err := part.Write(metadata); err != nil {
				return err
			}
			for _, e := range files {
				if err := ctx.Err(); err != nil {
					return err
				}
				header := make(textproto.MIMEHeader)
				header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": e.Path}))
				signature, delta := signatures[e.Path]
				encoding := "application/octet-stream"
				if delta {
					encoding = DeltaType
				}
				header.Set("Content-Type", encoding)
				part, err := body.CreatePart(header)
				if err != nil {
					return err
				}
				if open == nil {
					return errors.New("missing file source")
				}
				source, err := open(e)
				if err != nil {
					return err
				}
				if delta {
					err = transfer.Delta(ctx, io.NewSectionReader(spool, signature.offset, signature.size), progress.Reader(ctx, "Encoding delta", e.Path, e.Size, source), part)
				} else {
					_, err = io.Copy(part, progress.Reader(ctx, "Uploading", e.Path, e.Size, source))
				}
				err = errors.Join(err, source.Close())
				if err != nil {
					return err
				}
			}
			if err := body.Close(); err != nil {
				return err
			}
			progress.Report(ctx, progress.Event{Phase: "Waiting for server"})
			return nil
		}()
		writer.CloseWithError(err)
		done <- err
	}()
	r, err := c.request(ctx, "POST", projectPath(owner, name)+"/save", reader, body.FormDataContentType())
	var version model.Version
	if err == nil {
		err = decodeJSON(r.Body, &version)
		r.Body.Close()
	}
	reader.Close()
	streamErr := <-done
	if err != nil {
		if streamErr != nil && !errors.Is(streamErr, io.ErrClosedPipe) {
			err = errors.Join(err, streamErr)
		}
		return model.Version{}, err
	}
	// An accepted response is authoritative even when a concurrent retry caused
	// the server to respond before consuming this request's redundant file data.
	return version, nil
}
