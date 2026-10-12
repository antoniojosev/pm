package registry

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxComposeProjectLen bounds a compose project name. It ends up in container,
// network and volume names, which Docker limits well below this.
const MaxComposeProjectLen = 64

var composeProjectRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Compose names a docker compose stack. With it, pm starts the stack as
// `docker compose -f <file> -p <project>` and sets COMPOSE_PROJECT_NAME, so
// two copies of the same repository (two worktrees, say) get their own
// containers, networks and volumes instead of colliding on the directory name.
// The containers are attributed back to the pm project by the
// com.docker.compose.project label Docker puts on them.
type Compose struct {
	// File is the compose file, absolute or relative to the project's
	// directory. Empty lets compose look for its default file.
	File string `json:"file,omitempty"`
	// Project is the compose project name: lowercase letters, digits, '_' and
	// '-', starting with a letter or digit, at most 64 characters.
	Project string `json:"project"`
}

// Validate rejects what compose would not take as a project name, and a file
// that cannot travel through an argument or an environment variable.
func (c Compose) Validate() error {
	if c.Project == "" {
		return fmt.Errorf("compose.project is required: it names the containers, networks and volumes of the stack")
	}
	if len(c.Project) > MaxComposeProjectLen || !composeProjectRe.MatchString(c.Project) {
		return fmt.Errorf("invalid compose project %q: use lowercase letters, digits, '_' or '-', starting with a letter or digit, at most %d characters", c.Project, MaxComposeProjectLen)
	}
	if strings.ContainsFunc(c.File, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("invalid compose file %q: no control characters", c.File)
	}
	return nil
}

// FindByCompose returns the project whose compose project name is name: how a
// container carrying the com.docker.compose.project label is attributed to a
// pm project. Names compare case-insensitively. When several entries share one
// compose project (a configuration error, they would share containers) the
// first one wins.
func (s *Store) FindByCompose(name string) (Project, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.state.Projects {
		if p.Compose != nil && strings.EqualFold(p.Compose.Project, name) {
			return p, true
		}
	}
	return Project{}, false
}
