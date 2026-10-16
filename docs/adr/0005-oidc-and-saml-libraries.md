# 0005. OIDC and SAML libraries

- Status: Proposed
- Date: 2026-10-16

## Context

The control center signs people in with OIDC (generic OAuth2 providers through
OIDC) and SAML 2.0, plus local accounts for bootstrapping. Admin rights can
come from an identity-provider group or claim. We test against Keycloak (owner's
choice), which speaks both protocols. Sign-in libraries are security-critical;
we want small, maintained ones with a good advisory record, and no auth proxy
in front.

## Options

**OIDC**

1. `github.com/coreos/go-oidc/v3` (v3.21) with `golang.org/x/oauth2`. These
   are the standard: discovery, JWKS rotation, ID token verification. PKCE
   comes from `x/oauth2`.
2. `github.com/zitadel/oidc`. It does more (it's also an OP), so it has more
   surface than we need.

**SAML**

1. `github.com/crewjam/saml`. The most used, but no commits since May 2025,
   and it has had two critical signature-bypass advisories.
2. `github.com/russellhaering/gosaml2` (v0.12, August 2026), built on
   `goxmldsig`. It is an SP library only, it's maintained, and its XML
   signature validation is the part that has been audited and fixed most
   recently.
3. An external broker (Dex or Keycloak itself) in front, so we only speak
   OIDC. It's one less protocol in our code, but it's another component in
   the install, and the brief asks for SAML sign-in to the control center.

**Local accounts**

Passwords are hashed with argon2id (`golang.org/x/crypto/argon2`).

## Decision

- **OIDC:** go-oidc v3 and x/oauth2, authorization code flow with PKCE,
  `nonce` and `state` checks, and tokens kept server-side only. Group and
  role claims are read from the ID token or the userinfo endpoint, mapped
  through the `Platform` resource's role mappings.
- **SAML:** gosaml2 with goxmldsig, SP-initiated flow with signed AuthnRequests.
  We require signed assertions, check `InResponseTo` against a stored request
  ID, check the audience and time window, and reject unsigned or
  multiply-signed documents. IdP metadata comes from a URL (refreshed) or
  pasted XML. Group attributes are mapped like OIDC claims.
- **Local accounts:** argon2id. They exist for bootstrapping, and an admin
  can turn them off once an identity provider works (the wizard offers to).
- **Keycloak** runs in the dev environment (`make dev`) and in the e2e suite,
  with an OIDC client and a SAML client pre-imported from a realm file. These
  tests exercise group-to-admin mapping in both protocols.
- A fuzz test drives the SAML response parser with mutated, signed fixtures
  (signature wrapping, comment injection, duplicate assertions).

## Consequences

- Two well-scoped libraries, each about protocol mechanics only. Our code
  handles sessions and role mapping.
- If gosaml2 stalls, swapping it out affects one package
  (`internal/auth/saml`).
- Logout is local only in v1 (we end our session); SAML single logout and OIDC
  back-channel logout are later work.
