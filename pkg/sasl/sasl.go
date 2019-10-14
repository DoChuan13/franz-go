// Package sasl specifies interfaces that any sasl authentication must provide
// to interop with Kafka SASL.
package sasl

import "context"

// Session is an authentication session.
type Session interface {
	// Challenge is called with a server response. This must return
	// if the authentication is done, or, if not, the next message
	// to send.
	//
	// Returning an error stops the authentication flow.
	Challenge([]byte) (bool, []byte, error)
}

// Mechanism authenticates with SASL.
type Mechanism interface {
	// Name is the name of this SASL authentication mechanism.
	Name() string

	// Authenticate initializes an authentication session. If the mechanism
	// is a client-first authentication mechanism, this also returns the
	// first message to write.
	//
	// If initializing a session fails, this can return an error to stop
	// the authentication flow.
	//
	// The provided context can be used through the duration of the session.
	Authenticate(ctx context.Context) (Session, []byte, error)
}
