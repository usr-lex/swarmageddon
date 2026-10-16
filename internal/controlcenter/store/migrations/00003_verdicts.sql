-- +goose Up
-- Warden verdicts (ADR 0011), with the human decision next to each.
CREATE TABLE verdicts (
    id         text PRIMARY KEY,
    workspace  text NOT NULL,
    room       text NOT NULL,
    layer      text NOT NULL DEFAULT '',
    severity   integer NOT NULL DEFAULT 0,
    rule       text NOT NULL DEFAULT '',
    reason     text NOT NULL DEFAULT '',
    evidence   text NOT NULL DEFAULT '',
    action     text NOT NULL DEFAULT '',
    source     text NOT NULL DEFAULT '',
    at         timestamptz NOT NULL,
    decision   text NOT NULL DEFAULT '',
    decided_by text NOT NULL DEFAULT '',
    decided_at timestamptz,
    note       text NOT NULL DEFAULT ''
);
CREATE INDEX verdicts_ws_at ON verdicts (workspace, at);
CREATE INDEX verdicts_ws_room_at ON verdicts (workspace, room, at);

-- +goose Down
DROP TABLE verdicts;
