// Package version holds the build identity of pm and the HTTP API generation
// it serves. It has no dependencies so any package can import it.
package version

// Version is the pm release, stamped at build time
// (-X github.com/antoniojosev/pm/internal/version.Version=...).
var Version = "0.1.0"

// API is the generation of the HTTP API under /api/v1. It only changes with
// a breaking change, which would also move the routes to a new prefix.
const API = 1
