# Common instructions for every swarmageddon build agent

You are one of several engineers building swarmageddon in parallel. The owner approved the design; build it, fast. "Build fast, push often, fail fast."

Read first (in your worktree): docs/design.md (whole), docs/conventions.md, the ADRs relevant to you in docs/adr/, api/v1alpha1/*.go (CRD types), proto/swarm/**.proto and generated gen/swarm/** (Connect/protobuf Go code). The prior art lives read-only at /home/user/remote-labs (behavior reference only).
Research notes (verified facts with sources) are in /tmp/claude-0/-home-user/e6902579-249c-5c06-9dd7-61017a37495c/scratchpad/: research-claude-code.md, research-ore.md, research-forgejo.md, research-k8s-libs.md, plus scratchpad/schema, schema-exp, ts (ore app-server JSON schema and TS types for ore 1.161.0) and scratchpad/scripts.

Environment:
- Go: always `export GOTOOLCHAIN=go1.26.9 PATH=$PATH:$(go env GOPATH)/bin`. buf, protoc-gen-go, protoc-gen-connect-go, controller-gen are installed in $(go env GOPATH)/bin. Regenerate with `make generate` if you change protos or API types (only if your task owns them).
- Module: github.com/usr-lex/swarmageddon. Deps already in go.mod: controller-runtime v0.25.2 (k8s 0.37), connect v1.21.1, nats-server v2.15.1, nats.go v1.54.0, go-oidc v3.21.0, gosaml2 v0.12.0, pgx v5, goose v3, ulid v2, modelcontextprotocol/go-sdk, creack/pty, coder/websocket, webpush-go, BurntSushi/toml. You may `go get` more; never downgrade. Prefer the standard library.
- Network: proxy.golang.org, registry.npmjs.org, pypi, github.com release downloads, api.github.com, archive.ubuntu.com work. Docker Hub works through the mirror already configured in dockerd (mirror.gcr.io); dockerd is running (start it with `dockerd >/tmp/dockerd.log 2>&1 &` if `docker info` fails). codeberg.org, code.forgejo.org, forgejo.org, quay.io, registry.k8s.io, Fedora mirrors and ghcr.io blobs are BLOCKED: no Forgejo binary or image can be fetched here. Write Forgejo-dependent code against the verified API (research-forgejo.md) and test it with httptest fakes.
- 4 CPUs, 15 GB RAM. Don't run huge parallel builds.

Rules:
- Stay inside the directories your task owns. Don't edit api/, proto/, gen/, docs/design.md or other agents' directories; if you truly need a change there, make the smallest additive one and say so in your report. You may add to docs/conventions.md only if you must, and say so.
- Write idiomatic, boring Go with short doc comments; match docs/design.md. Smallest thing that meets each requirement; no speculative abstractions.
- Tests are required: unit tests for logic, envtest/httptest fakes where useful. `go vet ./yourpkgs/...` and `go test ./yourpkgs/...` must pass. Run `go build ./...` before each commit.
- Commit early and often in your worktree branch, with clear messages ending in:
  Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01HoBXSNopGDs6XChYAFgc9C
  Do NOT push; the lead merges your branch. Don't run `go mod tidy` if it would drop deps others need (tidy is fine; the lead re-tidies on merge).
- Don't guess undocumented behavior: read source or the research notes, and in your final report say plainly what you verified (ran) vs assumed.
- Final report (under 400 words): what you built (packages, binaries), how to run/test it, what is verified vs untested, anything another component must know (interfaces, flags, env), and known gaps. Include your worktree path and branch name.
