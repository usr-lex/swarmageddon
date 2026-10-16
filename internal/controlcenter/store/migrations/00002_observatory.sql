-- +goose Up
-- Swarm observatory (docs/design.md §8.7): what agents and people did,
-- never message bodies.
CREATE TABLE activity_events (
    id        bigserial PRIMARY KEY,
    ts        timestamptz NOT NULL,
    workspace text NOT NULL,
    actor     text NOT NULL,
    kind      text NOT NULL,
    target    text NOT NULL DEFAULT '',
    repo      text NOT NULL DEFAULT '',
    number    integer NOT NULL DEFAULT 0,
    summary   text NOT NULL DEFAULT '',
    ref_url   text NOT NULL DEFAULT '',
    value     bigint NOT NULL DEFAULT 0,
    -- Natural key of the source event (commit SHA, delivery, audit record),
    -- so replays and overlapping sources don't double count.
    dedup     text
);
CREATE UNIQUE INDEX activity_events_dedup ON activity_events (workspace, dedup) WHERE dedup IS NOT NULL;
CREATE INDEX activity_events_ws_ts ON activity_events (workspace, ts);
CREATE INDEX activity_events_ws_actor_ts ON activity_events (workspace, actor, ts);

-- Chronicler reports sent to status@<workspace>.
CREATE TABLE reports (
    id        bigserial PRIMARY KEY,
    workspace text NOT NULL,
    sender    text NOT NULL,
    ts        timestamptz NOT NULL,
    body      text NOT NULL,
    message_id text NOT NULL,
    UNIQUE (workspace, message_id)
);
CREATE INDEX reports_ws_ts ON reports (workspace, ts);

-- How far the messaging audit has been followed, per workspace.
CREATE TABLE activity_cursors (
    workspace text PRIMARY KEY,
    ts        timestamptz NOT NULL
);

-- +goose Down
DROP TABLE activity_cursors;
DROP TABLE reports;
DROP TABLE activity_events;
