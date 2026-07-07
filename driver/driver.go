package driver

// Driver is the interface for Session handlers.
//
// Read and Touch report a missing (or expired) session via their found
// return value rather than an error: "session is gone" is a normal outcome
// that callers may treat as a fresh session, while a non-nil error means the
// store itself failed and stored data must not be overwritten.
type Driver interface {
	// Close closes the session handler.
	Close() error
	// Destroy destroys the session with the given ID. Destroying a missing
	// session is not an error.
	Destroy(id string) error
	// Gc performs garbage collection on the session handler with the given maximum lifetime.
	Gc(maxLifetime int) error
	// Read returns the session data for the given ID. found is false when
	// the session does not exist or has expired; err is reserved for store
	// failures.
	Read(id string) (data string, found bool, err error)
	// Touch refreshes the session's last access time without reading or
	// writing data. found is false when the session does not exist; err is
	// reserved for store failures.
	Touch(id string) (found bool, err error)
	// Write writes the session data associated with the given ID.
	Write(id string, data string) error
}
