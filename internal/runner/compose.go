package runner

import (
	"path/filepath"

	"github.com/antoniojosev/pm/internal/registry"
)

// composeEnv names the variables pm sets for a compose stack.
const (
	EnvComposeProject = "COMPOSE_PROJECT_NAME"
	EnvComposeFile    = "COMPOSE_FILE"
)

// applyCompose returns the command and extra environment (KEY=VAL) that give a
// launch its compose project. COMPOSE_PROJECT_NAME is always set. When the
// command is `docker compose ...` (or `docker-compose ...`), pm also puts
// `-f <file>` and `-p <project>` right after it, so the project name holds
// even for a compose invocation that ignores the environment; any other
// command, `make up` say, gets the file through COMPOSE_FILE instead.
func applyCompose(cmd []string, c *registry.Compose) (out []string, env []string) {
	if c == nil {
		return cmd, nil
	}
	env = []string{EnvComposeProject + "=" + c.Project}
	i := composeWord(cmd)
	if i < 0 {
		if c.File != "" {
			env = append(env, EnvComposeFile+"="+c.File)
		}
		return cmd, env
	}
	out = append(out, cmd[:i+1]...)
	if c.File != "" {
		out = append(out, "-f", c.File)
	}
	out = append(out, "-p", c.Project)
	out = append(out, cmd[i+1:]...)
	return out, env
}

// composeWord returns the index of the argument that makes cmd a compose
// invocation ("compose" in `docker compose`, the program in `docker-compose`),
// or -1 when it is not one.
func composeWord(cmd []string) int {
	if len(cmd) == 0 {
		return -1
	}
	switch filepath.Base(cmd[0]) {
	case "docker":
		if len(cmd) >= 2 && cmd[1] == "compose" {
			return 1
		}
	case "docker-compose":
		return 0
	}
	return -1
}
