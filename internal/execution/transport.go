package execution

import "context"

// Transport manages connection lifecycle for an execution channel. It is
// created once (per channel) and produces long-lived Sessions, one per agent.
type Transport interface {
	// NewSession opens a persistent, stateful shell. Sessions are not safe for
	// concurrent use; callers must serialize access per session.
	NewSession(ctx context.Context) (Session, error)

	// Close releases the transport and any shared connection it owns.
	Close() error
}
