# Audit of swarmageddon at f7cb407 (design vs code)

## Top 10 issues for a real install / M1
1. Claim admin fails: control center ClusterRole (charts/swarmageddon/templates/control-center-rbac.yaml) has only `get` on platforms/status; k8s.SetAdminClaimed uses Status().Patch -> 403. No test claims admin.
2. Control center can't read/write many of its own Secrets: Role resourceNames lists 7; missing gateway-provider-*, oidc-*, notify-*, forge-webhook-<ws>, forge-reader, control-center-gateway, control-center-saml; ApplySecret Gets first -> Forbidden. saml-* ConfigMaps can't be created at all.
3. Messaging never ready: ClusterRole swarmageddon-messaging (internal/operator/manifests/templates/messaging.yaml) lacks roletemplates; directory/kube.go informs on RoleTemplates -> cache never syncs -> /readyz fails.
4. Warden can't reach messaging: messaging NetworkPolicy (networkpolicies.yaml) admits ws namespaces, system ns, messaging pods only — not warden pods (component: warden, platform ns).
5. ntfy publishing 403: notify.Dispatcher.ntfy sends no Authorization; control center never reads ntfy-publisher; ntfy deny-all.
6. Forge webhooks fail TLS: https://control-center.swarmageddon-system.svc has a private-CA cert; Forgejo doesn't trust the CA.
7. Spend: control center reads Secret control-center-gateway (masterKey); nothing creates it.
8. Idle rollouts never happen: room controller hard-codes Idle: NeverIdle{} (whenIdle policy only rolls on update-now).
9. M1 acceptance untested: e2e writes CRs directly (no claim, preflight, wizard API, delivery proof, person<->agent, review/merge, CI runner); Playwright only in mock mode and not in CI.
10. Repo credentials dropped: UI collects importCredential and mirrorToken but connect.ts sends empty strings, proto lacks fields; no RBAC to write credential Secrets in ws-*.

## Missing
- §3.2 cert-manager TLS not implemented in the operator.
- §9.6 Forgejo OIDC sign-in (add-oauth) missing; services.forgejo.oidcLogin no effect.
- §9.3 forgejo.EnsureSystemHook never called (no system webhook → protection on agent/person-created repos).
- §11.1 no NetworkPolicy limiting the control center's ingress to the ingress controller.
- §11.4 no log redaction, no e2e grep for leaked secrets, messaging doesn't refuse bodies containing issued tokens.
- §8.4 keystroke recording option absent.
- §10.3 roomCrashed from pod restarts (k8s events) absent; workspace-wide diskLow from node stats only in runner pool.

## Partial
- TODO cmd/control-center/main.go:165 notify.NewTracker(srv.Notify, nil) — idleWithOpenWork ignores open forge work.
- storm.coalesceBytes never written into room.json; room.json janitor and warden.reviewEveryToolCalls read but never written (§12 says janitor thresholds per Room).
- supervisor reads gateway_key only at start (rotation needs restart).
- web/src/pages/workspace/steps.tsx says "Agents can also create repositories later" (contradicts max_repo_creation 0).
- Controlcenter RPCs unused by web: GetChannelMessage, GetUnreadSummary, WatchVerdicts.

## Untested
internal/controlcenter/{k8s,audit,msgclient}, internal/supervisor/{display,engine,agent,llm}, internal/operator/apply, internal/warden/keytable, cmd/*. RBAC and NetworkPolicy reachability untested at any level.

## Inconsistent
- design §7.4 says person header "signed"; code trusts x-swarm-on-behalf-of from the control-center mTLS principal (conventions match code) → update design.
- design naming: §2 devicecheck (code: devices), §13 manifests/ and hack/mockmodel paths, §6.1 Fedora text.
