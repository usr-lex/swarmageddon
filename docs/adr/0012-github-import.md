# 0012. Importing a GitHub organization

- Status: Proposed (needs the owner's answers to the questions at the end)
- Date: 2026-10-19

## Context

The owner's previous lab ran on a GitHub organization: repositories,
issues, pull requests, reviews and comments, written by people and by one
GitHub account per agent, named `bot-<room>` (`bot-athena` for room
`athena`). They want to bring that history into a workspace, with each
agent's past posts attributed to the room that continues its work.

What Forgejo already does (checked in Gitea's source, which Forgejo forks;
to confirm against Forgejo 17 before building):

- **Repository migration** (`POST /repos/migrate`, `service: github`, a
  GitHub token) copies the git data and, on request, issues, pull requests
  (with their head refs), reviews, comments, labels, milestones, releases
  and the wiki, keeping numbers and timestamps.
- **Authors** are kept as the GitHub login and numeric user ID
  (`original_author`, `original_author_id`). The post shows as "originally
  by bot-athena", not as a Forgejo user.
- **Re-attribution** happens when a local user has a linked external login
  on a GitHub OAuth2 auth source whose external ID is that GitHub user ID:
  the cron task `update_migration_poster_id` (and linking an account) then
  rewrites issues, comments, reviews, reactions and releases to the local
  user (`services/externalaccount.UpdateMigrationsByType`).
- There is **no API to create that link**; it is made when a person signs
  in with GitHub, or by writing a row into `external_login_user`.

## Proposal

An **Import from GitHub** step in the workspace wizard (also callable as an
RPC), run by the operator as a WorkspaceImport resource so it survives
restarts and reports progress:

1. **Discover.** With a GitHub token (read-only: org, repos, issues, pull
   requests), list the organization's repositories and the accounts that
   posted. Accounts named `bot-<name>` are proposed as rooms named `<name>`
   (the prefix is configurable). Other accounts are listed for mapping to
   people.
2. **Plan.** The wizard shows the repositories to bring (all by default),
   the rooms it will create or reuse, and the people mapping. Room settings
   (CLI, model, role) come from the normal wizard; a room can be skipped.
3. **Migrate.** For each repository, a Forgejo migration with issues, pull
   requests, reviews, comments, labels, milestones, releases and wiki. The
   token is passed to Forgejo for the migration only and not stored.
4. **Attribute.** The operator creates a GitHub OAuth2 auth source in Forgejo
   (disabled for sign-in when the owner doesn't want GitHub logins) and, for
   each mapped account, links the room's or person's Forgejo user to the
   GitHub user ID (a row in `external_login_user`, written through the
   operator's Postgres access), then runs `update_migration_poster_id`
   (`POST /admin/cron/update_migration_poster_id`).
5. **Hand over.** Imported repositories get the workspace's branch
   protection, and each room is told in its first turn which repositories
   and open issues and pull requests it had (assignments carry over).

Things that don't carry over: GitHub Actions workflows run on Forgejo
Actions only where compatible; GitHub Apps, webhooks, secrets, projects
and discussions are not migrated; commit authorship stays the git author
(`bot-athena <...>`) as recorded in the commits.

## Alternatives considered

- **Replay through the API with `Sudo`**: each issue and comment created as
  the mapped user. Attribution is native, but timestamps become the import
  time, pull requests lose their review history, and a large organization
  takes hours and many API calls. Rejected unless the database link is
  ruled out.
- **Leave authors as "originally by"**: no database write, but the rooms
  don't own their history (mentions, assignments and "my open PRs" don't
  find it).

## Questions for the owner

1. One-time import, or keep GitHub and Forgejo in sync for a while (pull
   mirrors for code; issues and PRs can't be synced both ways)?
2. Is writing the account link directly into Forgejo's database acceptable
   (it is the only way to re-attribute without each account signing in)?
3. How should people's GitHub accounts map: to their sign-in identity
   (email), an explicit table in the wizard, or left as "originally by"?
4. Should rooms that exist only in GitHub (a `bot-` account with no room
   yet) be created by the import, or only mapped when you create them?
5. What GitHub access will you give the import: a fine-grained token or a
   GitHub App installation on the organization?
