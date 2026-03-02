package driver

// Driver is the interface for Session handlers.
type Driver interface {
	// Close closes the session handler.
	Close() error
	// Destroy destroys the session with the given ID.
	Destroy(id string) error
	// Gc performs garbage collection on the session handler with the given maximum lifetime.
	Gc(maxLifetime int) error
	// Read reads the session data associated with the given ID.
	Read(id string) (string, error)
	// Touch refreshes the session's last access time without reading or writing data.
	Touch(id string) error
	// Write writes the session data associated with the given ID.
	Write(id string, data string) error
}
