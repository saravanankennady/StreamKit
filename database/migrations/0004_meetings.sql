-- Meetings: who may run a room that people join through share links, and who may be in it.
--
-- A host link makes its holder a host. What anyone may do is read from meeting_members when they ask,
-- never from their token or their browser, so a removal or a change of role takes effect at once.
ALTER TABLE join_links ADD COLUMN role text NOT NULL DEFAULT 'guest';

CREATE TABLE meetings (
    project_id   uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
    room         text NOT NULL,
    title        text NOT NULL DEFAULT '',
    locked       boolean NOT NULL DEFAULT false,
    waiting_room boolean NOT NULL DEFAULT false,
    created_by   uuid REFERENCES users ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    ended_at     timestamptz,
    PRIMARY KEY (project_id, room)
);

-- One row per identity a share link has handed out. It outlives the session: a removed person's token is
-- good for hours, and this row is what refuses it.
CREATE TABLE meeting_members (
    project_id  uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
    room        text NOT NULL,
    identity    text NOT NULL,
    name        text NOT NULL DEFAULT '',
    role        text NOT NULL,
    state       text NOT NULL,
    link_id     uuid REFERENCES join_links ON DELETE SET NULL,
    -- Proves a place in the waiting room. Cleared once it has been swapped for a token.
    ticket_hash bytea UNIQUE,
    seen_at     timestamptz NOT NULL DEFAULT now(),
    joined_at   timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, room, identity)
);
CREATE INDEX meeting_members_waiting ON meeting_members (project_id, room) WHERE state = 'waiting';
CREATE INDEX meeting_members_created ON meeting_members (created_at);
