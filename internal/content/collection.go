package content

// Collection reports physical bytes in the native repository before and after
// a full native maintenance pass. It excludes the Kopia config and caches.
type Collection struct {
	BeforeBytes int64 `json:"before_bytes"`
	AfterBytes  int64 `json:"after_bytes"`
}
