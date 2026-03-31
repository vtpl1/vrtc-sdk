package av

// Closer is the single-method interface for releasing a resource.
// Implementations must be idempotent when called multiple times.
type Closer interface {
	Close() error
}
