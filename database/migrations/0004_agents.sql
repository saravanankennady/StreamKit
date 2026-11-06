-- Provider API keys for agents, and a record of what agent sessions did.
--
-- A provider key is a credential for someone else's paid service. It is stored sealed like an API secret, and no route
-- returns it to a browser. Only a worker holding the project's API secret can read it back.
CREATE TABLE provider_credentials (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id    uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
    provider      text NOT NULL,
    name          text NOT NULL DEFAULT 'default',
    secret_sealed bytea NOT NULL,
    created_by    uuid REFERENCES users ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz,
    UNIQUE (project_id, provider, name)
);

-- One row per agent session, as its worker reported it. The turns hold what people said, unless the worker left that out.
CREATE TABLE agent_sessions (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id     uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
    session_key    text NOT NULL,
    room           text NOT NULL DEFAULT '',
    job_id         text NOT NULL DEFAULT '',
    agent_name     text NOT NULL DEFAULT '',
    pipeline       text NOT NULL DEFAULT '',
    models         jsonb NOT NULL DEFAULT '{}',
    started_at     timestamptz NOT NULL,
    ended_at       timestamptz,
    turn_count     integer NOT NULL DEFAULT 0,
    interrupted    integer NOT NULL DEFAULT 0,
    error_count    integer NOT NULL DEFAULT 0,
    -- Average seconds from the end of the user's speech to the first reply audio, over turns that measured it.
    avg_end_to_end double precision,
    -- Only set when every turn's cost is known. A partial sum would read as the whole cost.
    cost_usd       double precision,
    body           jsonb NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, session_key)
);
CREATE INDEX agent_sessions_project ON agent_sessions (project_id, started_at DESC);
