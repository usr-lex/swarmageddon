package agent

import "strings"

// Restarter is implemented by drivers that can restart the CLI with changed
// environment variables, e.g. a rotated gateway key. The CLI resumes its
// session, and the restart is not reported as an exit. A CLI that isn't
// running gets the variables at its next start.
type Restarter interface {
	Restart(vars map[string]string)
}

// SetEnv returns env (KEY=value entries) with vars set: an existing entry is
// replaced, a new one appended. env itself is not modified.
func SetEnv(env []string, vars map[string]string) []string {
	out := make([]string, 0, len(env)+len(vars))
	done := map[string]bool{}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if v, ok := vars[k]; ok {
			if !done[k] {
				out = append(out, k+"="+v)
				done[k] = true
			}
			continue
		}
		out = append(out, kv)
	}
	for k, v := range vars {
		if !done[k] {
			out = append(out, k+"="+v)
		}
	}
	return out
}
