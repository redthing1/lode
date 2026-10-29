package model

// StorageUsage measures the content repository, excluding caches and SQLite.
type StorageUsage struct {
	Bytes     int64 `json:"bytes"`
	Allocated int64 `json:"allocated"`
}

type Collection struct {
	Before StorageUsage `json:"before"`
	After  StorageUsage `json:"after"`
}
