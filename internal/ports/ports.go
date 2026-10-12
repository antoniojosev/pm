// Package ports answers the one question the rest of pm cannot ask `ss`: can
// this process really bind a port right now.
package ports

import (
	"net"
	"strconv"
)

// Free reports whether port can be bound on every interface at this moment. It
// binds it and lets go straight away, so a port that `ss` does not list (a
// listener of another user or namespace, a socket in a state the parser skips)
// still counts as taken. A port that cannot be bound for any other reason, such
// as a privileged one without the capability, is not free either: pm could not
// hand it to a process running as this user.
//
// The answer is a snapshot. Between the bind test and the process that wants
// the port, anything can grab it, which is why leases exist: they stop pm's
// own callers from receiving the same port twice.
func Free(port int) bool {
	if port < 1 || port > 65535 {
		return false
	}
	l, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}
