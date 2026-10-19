// Package display runs the room's virtual display: Xvfb on :99 that only the
// agent user can use (remote-labs' room-xvfb), openbox, and x11vnc on
// localhost for the control center's noVNC.
package display

import "github.com/usr-lex/swarmageddon/internal/supervisor/proc"

// Display is the X display number used in rooms.
const Display = ":99"

// xvfbScript starts Xvfb with access control on and allows only the user it
// runs as (by local Unix user), so the agent's programs connect without a
// cookie while other users are refused. -noreset keeps the rule.
const xvfbScript = `set -eu
display="$1"; geometry="$2"
cookie="$(mktemp)"
xauth -q -f "$cookie" add "$display" . "$(mcookie)"
Xvfb "$display" -screen 0 "$geometry" -nolisten tcp -noreset -auth "$cookie" &
xvfb=$!
trap 'kill "$xvfb" 2>/dev/null' TERM INT
allowed=
for _ in $(seq 100); do
  if XAUTHORITY="$cookie" DISPLAY="$display" xhost "+si:localuser:$(id -un)" >/dev/null 2>&1; then allowed=1; break; fi
  kill -0 "$xvfb" 2>/dev/null || break
  sleep 0.1
done
rm -f "$cookie"
if [ -z "$allowed" ]; then echo "could not allow $(id -un) on $display" >&2; kill "$xvfb" 2>/dev/null; exit 1; fi
wait "$xvfb"
`

// Xvfb is the display server, run as the agent user.
func Xvfb(u *proc.User, geometry string) proc.Spec {
	if geometry == "" {
		geometry = "1920x1080x24"
	}
	return proc.Spec{Name: "xvfb", Path: "/bin/sh", Args: []string{"-c", xvfbScript, "room-xvfb", Display, geometry}, User: u}
}

// Openbox is the window manager.
func Openbox(u *proc.User) proc.Spec {
	return proc.Spec{Name: "openbox", Path: "openbox", Env: []string{"DISPLAY=" + Display}, User: u}
}

// X11VNC serves the display on 127.0.0.1:5900 without a password: only
// the control center reaches it, through the pods/portforward that needs the
// operator role and is audited; a password would protect nothing more, and
// nobody could type it (OpenShift report, issue 6).
func X11VNC(u *proc.User) proc.Spec {
	return proc.Spec{
		Name: "x11vnc", Path: "x11vnc", User: u,
		Args: []string{"-display", Display, "-localhost", "-rfbport", "5900", "-forever", "-shared", "-nopw", "-quiet"},
	}
}
