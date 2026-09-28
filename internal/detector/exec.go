package detector

import (
	"os/exec"
	"strings"
)

// run executes a command and returns trimmed stdout, or an error. It is a
// variable so tests can feed canned `ss` / `docker ps` output.
var run = func(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}
