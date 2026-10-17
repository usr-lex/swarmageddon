# Forgejo research for a K8s operator provisioning Forgejo for AI-agent teams (2026-10-16)

## Sources actually reachable / methodology
- codeberg.org, code.forgejo.org, forgejo.org, data.forgejo.org, artifacthub: **blocked** by egress proxy (403 / DNS fail). No GitHub mirror of forgejo/forgejo exists.
- **Primary source for API facts**: the Forgejo `swagger.v1.json` vendored in the Rust crate `forgejo-api` (by Cyborus, used by `fj`), downloaded from static.crates.io:
  - forgejo-api 0.12.0 -> spec `info.version = 17.0.0-dev-609-a14eb80977+gitea-1.22.0` (main branch shortly before v17)
  - older crates used for version gating: 0.8.0 = 13.0.1, 0.9.0 = 14.0.0, 0.10.0 = 15.0.0, 0.11.x = 16.0.0-dev-505.
- forgejo-runner source: Go module proxy `code.forgejo.org/forgejo/runner/v13@v13.2.0` (full zip).
- `fj` source: crate forgejo-cli 0.7.0. `tea` source: Go proxy `code.gitea.io/tea@v0.16.0`.
- Gitea source (github.com/go-gitea/gitea main + raw files at tag v1.21.11, the pre-hard-fork baseline Forgejo inherits) for behavior.
- WebSearch snippets for release schedule / helm chart.

Legend: **VERIFIED** = seen in spec/source; **INFERRED** = from Gitea source, Forgejo-adjacent evidence or prior knowledge, not confirmed in Forgejo source/docs.

## 0. Versions
- Forgejo tags in Go proxy (VERIFIED): `v15.0.10`, `v16.0.6`, `v17.0.0` are the latest tags per major; no v18. Module path is now `forgejo.org` (go.mod), so the Go proxy cannot serve the source.
- Release schedule (WebSearch of forgejo.org/docs/v17.0/admin/release-schedule): **v17.0 release date 15 Oct 2026**, EOL ~28 Jan 2027. So "current stable" = **v17.0.0** (released yesterday); v16.0.x previous stable. v15 is the LTS (INFERRED: LTS every 4th: v7, v11, v15; v15.0.10 patch count supports long support).
- Recommendation: target **Forgejo >= 16** minimum (needed for `/admin/users/{u}/tokens`), test against 17. Pin image `codeberg.org/forgejo/forgejo:17` / `:17-rootless` (INFERRED tag scheme; data.forgejo.org/forgejo/forgejo mirror also exists).
- Runner: **forgejo-runner v13.2.0** (2026-09-18, VERIFIED via Go proxy `code.forgejo.org/forgejo/runner/v13`); previous v12.13.2 (2026-07-23).
- tea: **v0.16.0** (2026-09-10, VERIFIED Go proxy). fj (forgejo-cli): **0.7.0** (VERIFIED crates index; date unknown).
- Auth headers (VERIFIED securityDefinitions): `Authorization: token <t>`, BasicAuth, `Sudo: <username>` header / `?sudo=` query (admin only), `X-FORGEJO-OTP` with basic auth when 2FA.

## 1. Admin / users
### Create user — `POST /admin/users` (VERIFIED), body `CreateUserOption` (required: `username`)
Fields: `username, email, full_name, login_name, password, must_change_password (bool), restricted (bool), send_notify (bool), source_id (int), visibility (string: public|limited|private), created_at`.
- No `max_repo_creation` / `prohibit_login` at create time -> follow with PATCH.
- Set `must_change_password: false` for agents (API default in Gitea is true when omitted — INFERRED from Gitea `CreateUser` handler: `MustChangePassword: true` unless explicitly false).

### Edit user — `PATCH /admin/users/{username}` (VERIFIED), body `EditUserOption`
Fields: `active, admin, allow_create_organization, allow_git_hook, allow_import_local, description, email, full_name, hide_email, location, login_name, max_repo_creation (int; -1 = use global default, 0 = none — INFERRED semantics), must_change_password, password, prohibit_login, pronouns, restricted, source_id, visibility, website`.
- Also VERIFIED: `POST /admin/users/{u}/rename`, `POST /admin/users/{u}/repos` (create repo owned by user), `POST /admin/users/{u}/orgs` (create org owned by user), `DELETE /admin/users/{u}?purge=`, `GET/DELETE /admin/users/{u}/emails`, quota endpoints `/admin/quota/*`, `/admin/users/{u}/quota`.

### Email notifications
- **No API** to set a user's email-notification preference: `UserSettingsOptions` (`PATCH /user/settings`) has only `description, diff_view_style, enable_repo_unit_hints, full_name, hide_activity, hide_email, hide_pronouns, language, location, pronouns, theme, website` (VERIFIED). Preference is web-UI only (`/user/settings/notifications`).
- Instance-level: `[admin] DEFAULT_EMAIL_NOTIFICATIONS = disabled` (values enabled|onmention|disabled|andyourown) sets the default for **new** users (VERIFIED in Gitea `modules/setting/admin.go`; INFERRED identical in Forgejo cheat sheet). Simplest for agent instances: `FORGEJO__mailer__ENABLED=false`, or create agents with an undeliverable domain + DEFAULT_EMAIL_NOTIFICATIONS=disabled.

### SSH keys
- `POST /admin/users/{username}/keys` body `CreateKeyOption {title*, key*, read_only}` (VERIFIED); `DELETE /admin/users/{u}/keys/{id}`.
- Or `POST /user/keys` with `Sudo: <user>`.

### GPG keys for another user
- **No admin endpoint** (VERIFIED: only `/user/gpg_keys` GET/POST, `/user/gpg_keys/{id}`, `/users/{u}/gpg_keys` GET, `/user/gpg_key_token` GET, `/user/gpg_key_verify` POST).
- Use Sudo: `POST /user/gpg_keys` + header `Sudo: agent-x`, body `CreateGPGKeyOption {armored_public_key*, armored_signature}`. To make the key "verified" API-only: `GET /user/gpg_key_token` (Sudo) -> sign token with the key (`gpg --armor --detach-sign`) -> pass as `armored_signature` on create, or `POST /user/gpg_key_verify {key_id*, armored_signature}`. (VERIFIED endpoints; flow INFERRED from Gitea `AddGPGKey(ctx, ownerID, content, token, signature)`.)
- GPG commit verification also requires the commit's committer email to be an activated email of the user and listed in the key UIDs (INFERRED, Gitea v1.21 gpg_key_commit_verification.go).

### SSH commit signing verification
- Supported (Forgejo inherits Gitea's sshsig verification). **But** (INFERRED from Gitea v1.21.11 `ParseCommitWithSSHSignature`, VERIFIED there): a key is only used if `k.Verified && committer email activated`. Keys added via API (`AddPublicKey(..., verified=false)`) are **unverified**.
- **No API endpoint to verify an SSH key** (VERIFIED absent in spec). Verification is web-UI only (Settings > SSH keys > Verify: sign a token with `ssh-keygen -Y sign -n gitea`). Options for the operator: (a) prefer **GPG signing** for agents (fully API-driven via Sudo, see above); (b) drive the web form with a session (fragile); (c) set `public_key.verified=true` in DB (unsupported). Worth confirming in Forgejo source (`models/asymkey/ssh_key_commit_verification.go`) whether Forgejo changed this.
- `require_signed_commits` on branch protection uses the same verification (pre-receive `verifyCommits`), so unverified SSH keys => pushes rejected.
- Instance signing keys: `GET /signing-key.gpg`, `GET /signing-key.ssh`, `GET /repos/{o}/{r}/signing-key.gpg` (VERIFIED) — for server-side commits (merges via UI) configured in `[repository.signing]`.

### Access tokens for another user
- **Forgejo >= 16: `POST /admin/users/{username}/tokens`** (`adminCreateUserAccessToken`), plus `GET` list and `DELETE /admin/users/{u}/tokens/{token}` (VERIFIED in 16.0.0-dev and 17 specs; absent in 15.0.0). Works with an admin **token** (no basic auth).
- `POST /users/{username}/tokens` (all versions): route has `reqSelfOrAdmin()` + `reqBasicOrRevProxyAuth()` (VERIFIED in Gitea; INFERRED same in Forgejo) => needs **Basic auth**; an admin's basic auth with a different `{username}` works; **Sudo with a token does not satisfy basic-auth requirement** (`ctx.IsBasicAuth` false -> 401).
- Body `CreateAccessTokenOption {name*, scopes []string, repositories []RepoTargetOption{owner*, name*}}`. `repositories` (repo-scoped tokens) is **Forgejo >= 15** (VERIFIED: absent in 14.0.0, present in 15.0.0). Response `AccessToken {id, name, sha1 (only on create), token_last_eight, scopes, repositories, created_at}`.
- Scope strings (INFERRED, Gitea model + Forgejo docs; spec has no enum): `all`, and `read:`/`write:` x `activitypub, admin, issue, misc, notification, organization, package, repository, user`. Gitea's `public-only` likely not in Forgejo (unverified). Agent token typical: `write:repository, write:issue, read:user, read:organization, read:package/write:package`.

## 2. Orgs & teams (VERIFIED)
- `POST /orgs` body `CreateOrgOption {username*, full_name, description, email, location, website, visibility: public|limited|private, repo_admin_change_team_access}`. Admin-owned alternative: `POST /admin/users/{u}/orgs`.
- `POST /orgs/{org}/teams` body `CreateTeamOption {name*, description, permission: read|write|admin, can_create_org_repo (bool), includes_all_repositories (bool), units []string (deprecated), units_map {unit: none|read|write|admin…}}`. Unit keys: `repo.code, repo.issues, repo.ext_issues, repo.wiki, repo.ext_wiki, repo.pulls, repo.releases, repo.projects, repo.packages, repo.actions`. `PATCH /teams/{id}` (EditTeamOption same fields).
- Members: `PUT /teams/{id}/members/{username}` (204), `DELETE ...`. Org membership is implied by team membership (INFERRED Gitea semantics). Team repos: `PUT /teams/{id}/repos/{org}/{repo}`, or `includes_all_repositories: true`.
- Restrict repo creation in org: `can_create_org_repo: false` on non-Owners teams (Owners team always can — INFERRED). Global: `[repository] MAX_CREATION_LIMIT`, `[service] DEFAULT_ALLOW_CREATE_ORGANIZATION=false`, per-user `allow_create_organization=false`, `max_repo_creation=0` (personal repos).

## 3. Branch protection (VERIFIED fields)
`POST /repos/{o}/{r}/branch_protections` (`PATCH/DELETE /branch_protections/{name}`) body `CreateBranchProtectionOption`:
`rule_name` (glob pattern; `branch_name` deprecated alias), `enable_push`, `enable_push_whitelist`, `push_whitelist_usernames`, `push_whitelist_teams`, `push_whitelist_deploy_keys`, `enable_merge_whitelist`, `merge_whitelist_usernames`, `merge_whitelist_teams`, `enable_status_check`, `status_check_contexts` (supports globs — INFERRED), `required_approvals`, `enable_approvals_whitelist`, `approvals_whitelist_username`, `approvals_whitelist_teams`, `block_on_rejected_reviews`, `block_on_official_review_requests`, `block_on_outdated_branch`, `dismiss_stale_approvals`, `ignore_stale_approvals`, `require_signed_commits`, `protected_file_patterns`, `unprotected_file_patterns`, `apply_to_admins`.
- **No `enable_force_push` / force-push allowlist / `block_admin_merge_override`** in Forgejo (VERIFIED absent; those are newer Gitea features).
- Force push and deletion: in the Gitea 1.21 baseline pre-receive hook, any protected branch is **unconditionally** protected from deletion and from force-push (VERIFIED in Gitea v1.21.11 `routers/private/hook_pre_receive.go`; INFERRED unchanged in Forgejo given no API fields). API branch delete (`DELETE /repos/{o}/{r}/branches/{b}`) also refuses protected branches (INFERRED).
- Tag protection exists: `/repos/{o}/{r}/tag_protections` (VERIFIED).
- **No org-wide or instance-wide branch protection / rulesets** (VERIFIED: no such paths). Operator must apply per repo (e.g., reconcile loop or system webhook `POST /admin/hooks` on repo-create events -> apply rules). Org template repos don't carry protection (INFERRED).

## 4. Forgejo Actions runners
- Runner v13.2.0 commands (VERIFIED source): `register` (deprecated; `--no-interactive --instance --token --name --labels --ephemeral`), `create-runner-file` (deprecated offline registration with shared secret), `daemon [--url --uuid --token-url --label (repeatable) --fetch-interval]`, `one-job [--wait --handle (Forgejo>=15) --url --uuid --token-url --label]`, `generate-config`, `cache-server`, `exec`, `validate`. Exit code 2 when one-job got no task.
- **New registration model (Forgejo >= 15, VERIFIED)**: `POST /admin/actions/runners`, `POST /orgs/{org}/actions/runners`, `POST /repos/{o}/{r}/actions/runners`, `POST /user/actions/runners` body `RegisterRunnerOptions {name*, description, ephemeral (bool)}` -> `RegisterRunnerResponse {id, uuid, token}`. Then run `forgejo-runner daemon|one-job --url URL --uuid UUID --token-url file:/secrets/token --label ...` — no `.runner` file needed. Also config `server.connections.<name>: {url, uuid, token | token_url, labels, fetch_interval}`.
- Registration-token endpoints still exist but **deprecated since Forgejo 15** (VERIFIED): `GET /admin/runners/registration-token`, `GET /orgs/{org}/actions/runners/registration-token`, `GET /repos/{o}/{r}/actions/runners/registration-token`, `GET /user/actions/runners/registration-token` -> `{token}`.
- Runner mgmt: `GET /…/actions/runners[?visible=]`, `GET/DELETE /…/actions/runners/{runner_id}`; `ActionRunner {id, uuid, name, description, labels, status: offline|idle|active, ephemeral, owner_id, repo_id, version}`.
- Pending jobs for autoscaling: `GET /orgs/{org}/actions/runners/jobs?labels=a,b` (also admin/repo/user variants) — runner-on-demand example polls this and spawns `one-job` runners (VERIFIED example `examples/on-demand`).
- **Ephemeral**: register with `ephemeral: true`; daemon mode refuses ephemeral connections ("not supported in daemon-mode", VERIFIED) -> use `one-job`. Ephemeral runners are removed and token invalidated after one job. Pairs naturally with a K8s Job per runner.
- K8s example (VERIFIED `examples/kubernetes/dind-docker.yaml`): Deployment with initContainer `register`, runner container (non-root, readOnlyRootFS) + `docker:dind` sidecar `privileged: true`, DOCKER_HOST=tcp://localhost:2376 with TLS certs in emptyDir. Uses old runner 6.4.0 image; no official Helm chart / ARC-style controller; no native Kubernetes executor (backends: `docker`, `lxc`, `host` — VERIFIED labels.go).
- Labels format (VERIFIED config example): `name:docker://image[?platform=linux/arm64]`, `name:host`, `name:lxc://debian:trixie`; or mapping form `{backend: docker, backend-options: {image, platform}}`.
- Job hooks / cleanup: no pre/post job hook config found in runner config (INFERRED absent). Server-side cleanup: `[cron.cleanup_offline_runners]` (referenced in example README, Forgejo >= 13). Ephemeral + one-job is the cleanup story.
- Runner image: `code.forgejo.org/forgejo/runner:13` (INFERRED tag; also data.forgejo.org/forgejo/runner).

## 5. Push mirrors, packages
- `POST /repos/{o}/{r}/push_mirrors` body `CreatePushMirrorOption {remote_address, remote_username, remote_password, interval (e.g. "8h0m0s"), sync_on_commit, branch_filter, use_ssh}` (VERIFIED; `use_ssh` makes Forgejo generate a keypair, `PushMirror.public_key` returned to install as deploy key). `GET /push_mirrors`, `GET/DELETE /push_mirrors/{name}`, `POST /push_mirrors-sync`. Requires `[mirror] ENABLED=true` (default, INFERRED). 413 when quota exceeded.
- Packages API (VERIFIED): `GET /packages/{owner}`, `GET/DELETE /packages/{owner}/{type}/{name}/{version}`, `/files`, link/unlink to repo `/packages/{owner}/{type}/{name}/-/link/{repo_name}`, `/-/unlink`.
- Registries (INFERRED, long-standing Forgejo feature): OCI container registry at `https://<host>/v2/`, images `<host>/<owner>/<image>:<tag>`; `docker login <host> -u <user> -p <token>` with `write:package`/`read:package`. Also npm, PyPI, Maven, Go, Cargo, Helm, generic, etc. Enabled by `[packages] ENABLED=true` (default). Actions jobs get an automatic token that can push packages to the repo owner (INFERRED).

## 6. Helm chart (code.forgejo.org/forgejo-helm/forgejo-helm) — mostly INFERRED (site blocked)
- OCI chart `oci://code.forgejo.org/forgejo-helm/forgejo`. Deploys Forgejo Deployment (or StatefulSet-like) + Services (http/ssh) + PVC + Ingress; init containers render `app.ini` from values (`gitea.config.*`) via environment-to-ini.
- Image defaults to **rootless** (`image.rootless: true`; HOME=/data/gitea/git, SSH_LISTEN_PORT 2222) — WebSearch snippet of README/values.
- Dependencies: historically Bitnami postgresql-ha (default) / postgresql, redis/valkey-cluster. A 2025-08-19 commit "drop built-in Valkey charts" was seen; Bitnami subcharts were dropped after Bitnami's 2025 image changes (INFERRED). Expect to bring your own Postgres (e.g., CloudNativePG).
- Config via env: `FORGEJO__<SECTION>__<KEY>` (e.g. `FORGEJO__database__DB_TYPE=postgres`, `FORGEJO__security__INSTALL_LOCK=true`) — VERIFIED usage in runner on-demand README; `.` in section names encoded as `_0X2E_` (INFERRED from Gitea environment-to-ini).
- DB: sqlite3, postgres, mysql/mariadb supported. SQLite fine for small/single-team instances; **Postgres recommended** for concurrent agent load (SQLite write locking under many agents + Actions).
- HA: chart README claims HA support "but not all implementation details of Forgejo core are officially HA-ready". Multi-replica requires RWX shared storage for repos, shared queue/cache/session (Redis/Valkey), external indexer, and cron leader concerns. **Recommend single replica** (`replicaCount: 1`, `strategy: Recreate`).
- Operator choice: rendering our own Deployment is reasonable; first admin via `forgejo admin user create --admin --username ... --password ... --email ...` (exec / init container) — VERIFIED pattern in runner example.

## 7. Agent CLIs
| | `fj` (forgejo-cli 0.7.0) | `tea` (v0.16.0, 2026-09-10) |
|---|---|---|
| Source | codeberg.org/forgejo-contrib/forgejo-cli (moved from Cyborus), Rust | gitea.com/gitea/tea, Go |
| Commands | repo, issue, pr, milestone, wiki, actions (secrets/vars), auth, release, tag, user, org (team), whoami | issues, pulls, releases, labels, milestones, repos, branches, orgs, actions, webhooks, notifications, admin, comment, times, `tea api` raw |
| PR review | `fj pr review [id] list|request` only (**cannot approve/request changes**) VERIFIED | `tea pulls approve|reject|review|review_comments|reply|resolve|unresolve|merge|checkout` VERIFIED |
| Non-interactive auth | `fj -H <host> auth add-token <TOKEN>` (writes keys file) VERIFIED | `GITEA_INSTANCE_URL` + `GITEA_TOKEN` env (no config needed) or `tea login add --url --token` (env `GITEA_SERVER_URL/TOKEN`) VERIFIED |
| Output | human | `--output json|yaml|csv|...` VERIFIED |
| arm64 | INFERRED: Codeberg releases have limited targets; `cargo install forgejo-cli` | INFERRED: official linux-arm64 binaries at dl.gitea.com |
| Forgejo compat | native (built on Forgejo spec) | works (Forgejo reports `+gitea-1.22.0` compat version; `--no-version-check` available) INFERRED |
- Recommendation: `tea` for agents (JSON output, review actions, env-var login); keep raw API via curl / `tea api` for gaps; `fj` as secondary.

## 8. SSO (OIDC / Keycloak)
- Forgejo supports OAuth2/OIDC auth sources (provider `openidConnect`) — INFERRED (core feature).
- **No API to create auth sources** (VERIFIED: no auth-source paths in spec; only `/user/applications/oauth2` = Forgejo acting as OAuth provider).
- Create via CLI: `forgejo admin auth add-oauth --name keycloak --provider openidConnect --key <client-id> --secret <secret> --auto-discover-url https://kc/realms/<r>/.well-known/openid-configuration --scopes "openid email profile groups" --group-claim-name groups --admin-group forgejo-admins --restricted-group ... --skip-local-2fa` ; `update-oauth --id N`; `forgejo admin auth list`. (Flag names VERIFIED in Gitea `cmd/admin_auth_oauth.go`; INFERRED Forgejo same, minus newest Gitea-only flags like `--ssh-public-key-claim-name`, `--full-name-claim-name`.)
- Related app.ini: `[oauth2_client] ENABLE_AUTO_REGISTRATION=true, USERNAME=nickname|preferred_username|email, ACCOUNT_LINKING=login|auto|disabled, UPDATE_AVATAR`; `[service] ALLOW_ONLY_EXTERNAL_REGISTRATION=true`, `DISABLE_REGISTRATION`. Operator can run the CLI via `kubectl exec`/Job sharing the data volume, idempotently (list -> add or update).
- Agents should stay local users (`login_name`/password-less, token-only; `prohibit_login` blocks web+API? — INFERRED: prohibit_login blocks sign-in incl. token API, so don't set it on agents).

## Open items to verify once code.forgejo.org is reachable
1. Forgejo SSH key verification (any API? auto-verify for admin-added keys?).
2. Exact token scope list in Forgejo 17 (`models/auth/access_token_scope.go`).
3. Helm chart current values (deps, default DB, rootless) at latest tag.
4. fj/tea arm64 release artifacts.
5. `must_change_password` default when omitted on POST /admin/users in Forgejo.
