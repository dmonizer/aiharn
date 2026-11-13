package filetools

// Limits bounds the text-file tools. Every value is a positive integer; zero
// values are replaced by the defaults when a Session is created.
type Limits struct {
	// MaxReadLines caps the number of lines one read may return.
	MaxReadLines int
	// MaxReadBytes caps the returned file content of one read, measured before
	// any JSON escaping.
	MaxReadBytes int64
	// MaxWriteBytes caps the content of a write.
	MaxWriteBytes int64
	// MaxFileBytes caps an existing file; larger files cannot be read or edited.
	MaxFileBytes int64
	// MaxEditResultBytes caps the file size an edit may produce.
	MaxEditResultBytes int64
}

// Built-in limits, matching the documented defaults.
const (
	DefaultMaxReadLines       = 500
	DefaultMaxReadBytes       = 262144
	DefaultMaxWriteBytes      = 1048576
	DefaultMaxFileBytes       = 10485760
	DefaultMaxEditResultBytes = 10485760
)

// DefaultLimits returns the built-in limits.
func DefaultLimits() Limits {
	return Limits{
		MaxReadLines:       DefaultMaxReadLines,
		MaxReadBytes:       DefaultMaxReadBytes,
		MaxWriteBytes:      DefaultMaxWriteBytes,
		MaxFileBytes:       DefaultMaxFileBytes,
		MaxEditResultBytes: DefaultMaxEditResultBytes,
	}
}

// withDefaults replaces non-positive fields with their defaults.
func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxReadLines <= 0 {
		l.MaxReadLines = d.MaxReadLines
	}
	if l.MaxReadBytes <= 0 {
		l.MaxReadBytes = d.MaxReadBytes
	}
	if l.MaxWriteBytes <= 0 {
		l.MaxWriteBytes = d.MaxWriteBytes
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = d.MaxFileBytes
	}
	if l.MaxEditResultBytes <= 0 {
		l.MaxEditResultBytes = d.MaxEditResultBytes
	}
	return l
}
