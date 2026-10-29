// Package remote implements the authenticated client protocol.
package remote

import (
	"errors"
	"github.com/redthing1/lode/internal/model"
)

const MaxMetadata = 16 << 20
const DeltaType = "application/vnd.lode.rsync"

const DownloadBatchFiles = 128
const DownloadBatchBytes = 8 << 20
const DownloadBatchFileSize = 1 << 20

func ValidateDownloadBatch(files []model.Entry) error {
	if len(files) == 0 || len(files) > DownloadBatchFiles {
		return errors.New("invalid download batch length")
	}
	if err := model.ValidateChanges(files); err != nil {
		return err
	}
	var total int64
	for _, e := range files {
		if e.Kind != model.File || e.Size > DownloadBatchFileSize {
			return errors.New("batch requires small files")
		}
		total += e.Size
	}
	if total > DownloadBatchBytes {
		return errors.New("download batch exceeds size limit")
	}
	return nil
}

type Identity struct {
	Instance string `json:"instance"`
	User     string `json:"user"`
	Username string `json:"username"`
}
type Project struct {
	ID    string `json:"id"`
	Owner string `json:"owner"`
	Name  string `json:"name"`
}
type Head struct {
	Version string `json:"version"`
}
type InventoryRecord struct {
	Entry *model.Entry `json:"entry,omitempty"`
	Count *int         `json:"count,omitempty"`
}

type Error struct {
	Code      string           `json:"code"`
	Message   string           `json:"message"`
	Status    int              `json:"-"`
	Conflicts []model.Conflict `json:"conflicts,omitempty"`
}

func (e *Error) Error() string { return e.Message }
