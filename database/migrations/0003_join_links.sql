-- Share links: anyone holding one can join a single room without an account.
--
-- The code in a link is a credential. Lookups use its hash, and the sealed copy exists only so the
-- dashboard can show the link again.
CREATE TABLE join_links (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
    room        text NOT NULL,
    label       text NOT NULL DEFAULT '',
    code_hash   bytea NOT NULL UNIQUE,
    code_sealed bytea NOT NULL,
    can_publish boolean NOT NULL DEFAULT true,
    uses        bigint NOT NULL DEFAULT 0,
    created_by  uuid REFERENCES users ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    revoked_at  timestamptz
);
CREATE INDEX join_links_project ON join_links (project_id, created_at DESC);
