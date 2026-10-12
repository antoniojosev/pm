package client

import (
	"errors"
	"fmt"
)

// ErrIncompatible is matched (errors.Is) by every error that says the daemon
// does not speak the API generation this client understands.
var ErrIncompatible = errors.New("incompatible pm API")

// VersionError reports a daemon this client cannot talk to. Got is 0 when the
// daemon does not answer GET /api/v1/version at all, which is how a pm older
// than API v1 looks.
type VersionError struct {
	// Want is the API generation the client speaks: APIVersion.
	Want int
	// Got is the one the daemon reported.
	Got int
	// PM is the daemon's release, when it reported one.
	PM string
}

func (e *VersionError) Error() string {
	if e.Got == 0 {
		return fmt.Sprintf("pm daemon does not serve API v%d (no answer on /api/v1/version): upgrade pm to a release that does", e.Want)
	}
	pm := ""
	if e.PM != "" {
		pm = " (pm " + e.PM + ")"
	}
	return fmt.Sprintf("pm daemon speaks API v%d%s but this client speaks API v%d: use a client that matches the daemon", e.Got, pm, e.Want)
}

// Is makes errors.Is(err, ErrIncompatible) true.
func (e *VersionError) Is(target error) bool { return target == ErrIncompatible }

// Error is a failed request: the daemon answered with a non-2xx status.
// Message is the text pm gave ({"error": ...}), or the raw body when it was not
// JSON.
type Error struct {
	Method  string
	Path    string
	Status  int
	Message string

	body []byte // the raw answer, for callers in this package
}

func (e *Error) Error() string {
	return fmt.Sprintf("pm: %s %s: %d: %s", e.Method, e.Path, e.Status, e.Message)
}

// decodeError is a 2xx answer that is not the JSON the API promises: a
// dashboard's index.html, a proxy's page. Version reads it as a daemon that
// does not serve the API.
type decodeError struct{ err error }

func (e *decodeError) Error() string { return e.err.Error() }
func (e *decodeError) Unwrap() error { return e.err }
