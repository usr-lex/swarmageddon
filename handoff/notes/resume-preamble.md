# Resuming after a container restart

The container restarted (out of memory) while you were working. Your earlier work is in your worktree: committed commits on your branch, plus possibly uncommitted changes. Start with:
  cd <your worktree>; git status; git log --oneline -15; git diff --stat
Review the uncommitted changes, keep what's good (commit it), and continue the task.

Memory rules (15 GB shared with 3 other agents):
- export GOTOOLCHAIN=go1.26.9 GOFLAGS=-p=2 GOMAXPROCS=2 PATH=$PATH:$(go env GOPATH)/bin
- Run tests only for your own packages; `-race` only on your own packages, one package at a time.
- No parallel docker builds; remove containers/images you create when done. Keep disk use low (disk is shared, ~10 GB free).
- Don't run the full repository test suite.

Before anything else, merge the lead branch to pick up new API and docs: `git merge claude/compassionate-wozniak-y0fs0x` (it is at 83e0a43 or later). Read the new design sections relevant to you: §6.5, §6.6, §7.8, §8.7, §9.7, §11.2 and ADRs 0010, 0011.

Everything in /tmp/claude-0/-home-user/e6902579-249c-5c06-9dd7-61017a37495c/scratchpad/agent-preamble.md still applies (read it), except: commit in your worktree, don't push.
