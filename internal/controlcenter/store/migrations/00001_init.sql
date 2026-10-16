-- +goose Up
CREATE TABLE users (
    id            text PRIMARY KEY,
    handle        text NOT NULL UNIQUE,
    email         text NOT NULL DEFAULT '',
    display_name  text NOT NULL DEFAULT '',
    provider      text NOT NULL,
    mapped_role   text NOT NULL DEFAULT '',
    role_override text,
    disabled      boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_login    timestamptz
);

CREATE TABLE identities (
    provider   text NOT NULL,
    subject    text NOT NULL,
    user_id    text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    groups     text[] NOT NULL DEFAULT '{}',
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, subject)
);

CREATE TABLE sessions (
    id_hash    bytea PRIMARY KEY,
    user_id    text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL,
    last_seen  timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    source_ip  text NOT NULL DEFAULT '',
    user_agent text NOT NULL DEFAULT ''
);
CREATE INDEX sessions_expires_at ON sessions (expires_at);

-- Workspaces a user owns (created through the wizard) or was assigned.
CREATE TABLE workspace_assignments (
    user_id    text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    workspace  text NOT NULL,
    kind       text NOT NULL DEFAULT 'member',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, workspace)
);

CREATE TABLE audit_log (
    id        bigserial PRIMARY KEY,
    at        timestamptz NOT NULL,
    actor     text NOT NULL,
    action    text NOT NULL,
    object    text NOT NULL DEFAULT '',
    workspace text NOT NULL DEFAULT '',
    source_ip text NOT NULL DEFAULT '',
    result    text NOT NULL DEFAULT 'ok',
    before    text NOT NULL DEFAULT '',
    after     text NOT NULL DEFAULT '',
    detail    text NOT NULL DEFAULT ''
);
CREATE INDEX audit_log_at ON audit_log (at);
CREATE INDEX audit_log_workspace_at ON audit_log (workspace, at);

CREATE TABLE approvals (
    id                      text PRIMARY KEY,
    workspace               text NOT NULL,
    room                    text NOT NULL,
    cli                     text NOT NULL DEFAULT '',
    tool                    text NOT NULL DEFAULT '',
    summary                 text NOT NULL DEFAULT '',
    detail                  text NOT NULL DEFAULT '',
    reason                  text NOT NULL DEFAULT '',
    session_scope_available boolean NOT NULL DEFAULT false,
    requested_at            timestamptz NOT NULL,
    expires_at              timestamptz,
    state                   text NOT NULL,
    decision                text NOT NULL DEFAULT '',
    answered_by             text NOT NULL DEFAULT '',
    answer_message          text NOT NULL DEFAULT '',
    answered_at             timestamptz
);
CREATE INDEX approvals_workspace_requested ON approvals (workspace, requested_at);

CREATE TABLE webpush_subscriptions (
    endpoint   text PRIMARY KEY,
    user_id    text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    p256dh     text NOT NULL,
    auth       text NOT NULL,
    user_agent text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE notification_prefs (
    user_id   text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    workspace text NOT NULL DEFAULT '',
    event     text NOT NULL,
    channels  text[] NOT NULL DEFAULT '{}',
    PRIMARY KEY (user_id, workspace, event)
);

-- Messages to and from people, cached from messaging.
CREATE TABLE inbox (
    user_id   text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    id        text NOT NULL,
    workspace text NOT NULL,
    from_addr text NOT NULL,
    to_addr   text NOT NULL,
    body      text NOT NULL,
    reply_to  text NOT NULL DEFAULT '',
    sent_at   timestamptz NOT NULL,
    direction text NOT NULL,
    read      boolean NOT NULL DEFAULT false,
    PRIMARY KEY (user_id, id)
);
CREATE INDEX inbox_user_sent ON inbox (user_id, sent_at);

-- Outstanding SAML AuthnRequest IDs, for the InResponseTo check across replicas.
CREATE TABLE saml_requests (
    id         text PRIMARY KEY,
    provider   text NOT NULL,
    expires_at timestamptz NOT NULL
);

-- +goose Down
DROP TABLE saml_requests;
DROP TABLE inbox;
DROP TABLE notification_prefs;
DROP TABLE webpush_subscriptions;
DROP TABLE approvals;
DROP TABLE audit_log;
DROP TABLE workspace_assignments;
DROP TABLE sessions;
DROP TABLE identities;
DROP TABLE users;
