package web

import (
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/redthing1/lode/internal/instance"
	"github.com/redthing1/lode/internal/project"
)

func mediaType(name string) (kind, mime string) {
	switch strings.ToLower(path.Ext(name)) {
	case ".png":
		return "image", "image/png"
	case ".jpg", ".jpeg":
		return "image", "image/jpeg"
	case ".gif":
		return "image", "image/gif"
	case ".webp":
		return "image", "image/webp"
	case ".avif":
		return "image", "image/avif"
	case ".mp3":
		return "audio", "audio/mpeg"
	case ".wav":
		return "audio", "audio/wav"
	case ".ogg", ".opus":
		return "audio", "audio/ogg"
	case ".flac":
		return "audio", "audio/flac"
	case ".m4a":
		return "audio", "audio/mp4"
	case ".mp4":
		return "video", "video/mp4"
	case ".webm":
		return "video", "video/webm"
	case ".ogv":
		return "video", "video/ogg"
	}
	return "", ""
}

func mediaURL(p instance.Project, version, name string) string {
	u := url.URL{Path: projectURL(p) + "/media/" + version + "/" + name}
	return u.String()
}

func (s *Server) media(w http.ResponseWriter, r *http.Request, actor instance.Principal) error {
	return s.serveFile(w, r, actor, true)
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, actor instance.Principal, inline bool) error {
	return s.instance.WithProject(r.Context(), actor, r.PathValue("owner"), r.PathValue("project"), instance.Reader, func(_ instance.Project, p *project.Store) error {
		e, reader, err := p.File(r.Context(), r.PathValue("version"), r.PathValue("path"))
		if err != nil {
			return err
		}
		defer reader.Close()
		mime, disposition := "application/octet-stream", "attachment"
		if inline {
			_, mime = mediaType(e.Path)
			if mime == "" {
				http.Error(w, "This file has no inline preview.", http.StatusUnsupportedMediaType)
				return nil
			}
			if strings.Contains(r.Header.Get("Range"), ",") {
				w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(e.Size, 10))
				http.Error(w, "Only one byte range is supported.", http.StatusRequestedRangeNotSatisfiable)
				return nil
			}
			disposition = "inline"
		}
		w.Header().Set("Content-Type", mime)
		w.Header().Set("Content-Disposition", disposition)
		http.ServeContent(w, r, path.Base(e.Path), time.Time{}, reader)
		return nil
	})
}
