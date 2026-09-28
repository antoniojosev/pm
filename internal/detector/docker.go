package detector

import (
	"encoding/json"
	"strconv"
	"strings"
)

// dockerPS is the subset of `docker ps --format json` we consume.
type dockerPS struct {
	Names  string `json:"Names"`
	Ports  string `json:"Ports"`
	Labels string `json:"Labels"`
	ID     string `json:"ID"`
}

// dockerListeners queries docker for published ports. Silent if docker absent.
func (d Detector) dockerListeners() []Instance {
	out, err := run("docker", "ps", "--format", "{{json .}}")
	if err != nil {
		return nil
	}
	return parseDockerPS(out)
}

// parseDockerPS converts `docker ps --format '{{json .}}'` lines into one
// Instance per published host port, attributed to the compose project (or
// the container name when the container is not part of a compose stack).
func parseDockerPS(out string) []Instance {
	var res []Instance
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var c dockerPS
		if json.Unmarshal([]byte(line), &c) != nil {
			continue
		}
		name := composeProject(c.Labels)
		if name == "" {
			name = c.Names
		}
		for _, port := range publishedPorts(c.Ports) {
			container := c.ID
			if container == "" {
				container = c.Names
			}
			res = append(res, Instance{
				Project:   name,
				Port:      port,
				Cmd:       "docker: " + c.Names,
				Source:    SourceDocker,
				Docker:    true,
				Container: container,
			})
		}
	}
	return res
}

// composeProject extracts com.docker.compose.project from the labels blob.
func composeProject(labels string) string {
	for _, kv := range strings.Split(labels, ",") {
		if v, ok := strings.CutPrefix(kv, "com.docker.compose.project="); ok {
			return v
		}
	}
	return ""
}

// publishedPorts parses "0.0.0.0:18000->8000/tcp, :::18000->8000/tcp".
func publishedPorts(ports string) []int {
	seen := map[int]bool{}
	var out []int
	for _, seg := range strings.Split(ports, ",") {
		seg = strings.TrimSpace(seg)
		arrow := strings.Index(seg, "->")
		if arrow < 0 {
			continue
		}
		host := seg[:arrow]
		if i := strings.LastIndex(host, ":"); i >= 0 {
			host = host[i+1:]
		}
		if n, err := strconv.Atoi(host); err == nil && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}
