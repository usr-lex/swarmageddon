// Package engine runs Podman's Docker-compatible API service (ADR 0001).
package engine

import "github.com/usr-lex/swarmageddon/internal/supervisor/proc"

// DockerHost is the address the docker CLI uses inside a room.
const DockerHost = "unix:///var/run/docker.sock"

// Spec runs `podman system service --time=0` on the Docker socket, as root.
func Spec() proc.Spec {
	return proc.Spec{
		Name: "podman",
		Path: "podman",
		Args: []string{"system", "service", "--time=0", DockerHost},
	}
}
