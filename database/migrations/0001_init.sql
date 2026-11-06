-- Accounts and tenancy.
CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE users (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email             citext NOT NULL UNIQUE,
    name              text NOT NULL DEFAULT '',
    password_hash     text NOT NULL,
    is_platform_admin boolean NOT NULL DEFAULT false,
    email_verified_at timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    token_hash   bytea NOT NULL UNIQUE,
    user_agent   text NOT NULL DEFAULT '',
    ip           text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL
);
CREATE INDEX sessions_user ON sessions (user_id);
CREATE INDEX sessions_expiry ON sessions (expires_at);

-- One-time tokens for email verification and password reset. Only hashes are stored.
CREATE TABLE email_tokens (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    purpose    text NOT NULL CHECK (purpose IN ('verify_email', 'reset_password')),
    token_hash bytea NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_at    timestamptz
);

CREATE TABLE organizations (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE memberships (
    org_id     uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users ON DELETE CASCADE,
    role       text NOT NULL CHECK (role IN ('owner', 'admin', 'member')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id)
);
CREATE INDEX memberships_user ON memberships (user_id);

CREATE TABLE projects (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    name        text NOT NULL,
    region      text NOT NULL DEFAULT 'default',
    environment text NOT NULL DEFAULT 'development' CHECK (environment IN ('development', 'staging', 'production')),
    status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
    settings    jsonb NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    deleted_at  timestamptz
);
CREATE INDEX projects_org ON projects (org_id) WHERE deleted_at IS NULL;

-- API secrets must stay readable to verify HS256 tokens, so they are encrypted, not hashed. The key comes
-- from the environment and never reaches this database.
CREATE TABLE api_keys (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id        uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
    api_key           text NOT NULL UNIQUE,
    secret_ciphertext bytea NOT NULL,
    name              text NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now(),
    expires_at        timestamptz,
    revoked_at        timestamptz
);
CREATE INDEX api_keys_project ON api_keys (project_id);

-- Session history. Live state stays in memory on the node hosting the room.
CREATE TABLE rooms (
    sid               text PRIMARY KEY,
    project_id        text NOT NULL,
    name              text NOT NULL,
    node_id           text NOT NULL DEFAULT '',
    metadata          text NOT NULL DEFAULT '',
    max_participants  integer NOT NULL DEFAULT 0,
    peak_participants integer NOT NULL DEFAULT 0,
    total_participants integer NOT NULL DEFAULT 0,
    created_at        timestamptz NOT NULL DEFAULT now(),
    ended_at          timestamptz
);
CREATE INDEX rooms_project_created ON rooms (project_id, created_at DESC);
CREATE INDEX rooms_project_name ON rooms (project_id, name);

CREATE TABLE participant_sessions (
    sid               text PRIMARY KEY,
    room_sid          text NOT NULL REFERENCES rooms ON DELETE CASCADE,
    project_id        text NOT NULL,
    identity          text NOT NULL,
    name              text NOT NULL DEFAULT '',
    kind              text NOT NULL DEFAULT 'STANDARD',
    joined_at         timestamptz NOT NULL DEFAULT now(),
    left_at           timestamptz,
    disconnect_reason text NOT NULL DEFAULT ''
);
CREATE INDEX participant_sessions_room ON participant_sessions (room_sid);
CREATE INDEX participant_sessions_project ON participant_sessions (project_id, joined_at DESC);

CREATE TABLE track_sessions (
    sid             text PRIMARY KEY,
    participant_sid text NOT NULL,
    room_sid        text NOT NULL REFERENCES rooms ON DELETE CASCADE,
    project_id      text NOT NULL,
    type            text NOT NULL,
    source          text NOT NULL,
    mime_type       text NOT NULL DEFAULT '',
    published_at    timestamptz NOT NULL DEFAULT now(),
    unpublished_at  timestamptz
);
CREATE INDEX track_sessions_room ON track_sessions (room_sid);

CREATE TABLE webhooks (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid NOT NULL REFERENCES projects ON DELETE CASCADE,
    url        text NOT NULL,
    -- Deliveries are signed with this key, so receivers verify with the same credentials as everything else.
    api_key_id uuid NOT NULL REFERENCES api_keys ON DELETE CASCADE,
    events     text[] NOT NULL DEFAULT '{}',
    enabled    boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX webhooks_project ON webhooks (project_id);

CREATE TABLE webhook_deliveries (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    webhook_id      uuid NOT NULL REFERENCES webhooks ON DELETE CASCADE,
    event_type      text NOT NULL,
    payload         jsonb NOT NULL,
    status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'failed')),
    attempts        integer NOT NULL DEFAULT 0,
    response_status integer,
    last_error      text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    delivered_at    timestamptz
);
CREATE INDEX webhook_deliveries_webhook ON webhook_deliveries (webhook_id, created_at DESC);
CREATE INDEX webhook_deliveries_due ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';

CREATE TABLE egresses (
    id           text PRIMARY KEY,
    project_id   text NOT NULL,
    room_name    text NOT NULL,
    room_sid     text NOT NULL DEFAULT '',
    type         text NOT NULL,
    status       text NOT NULL,
    request      jsonb NOT NULL DEFAULT '{}',
    output_path  text NOT NULL DEFAULT '',
    output_url   text NOT NULL DEFAULT '',
    format       text NOT NULL DEFAULT '',
    file_size    bigint NOT NULL DEFAULT 0,
    duration_ms  bigint NOT NULL DEFAULT 0,
    error        text NOT NULL DEFAULT '',
    retry_of     text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    started_at   timestamptz,
    ended_at     timestamptz
);
CREATE INDEX egresses_project ON egresses (project_id, created_at DESC);

CREATE TABLE ingresses (
    id                    text PRIMARY KEY,
    project_id            text NOT NULL,
    name                  text NOT NULL DEFAULT '',
    input_type            text NOT NULL CHECK (input_type IN ('rtmp', 'whip', 'url')),
    room_name             text NOT NULL,
    participant_identity  text NOT NULL,
    participant_name      text NOT NULL DEFAULT '',
    stream_key_hash       bytea,
    stream_key_ciphertext bytea,
    source_url            text NOT NULL DEFAULT '',
    enabled               boolean NOT NULL DEFAULT true,
    status                text NOT NULL DEFAULT 'inactive',
    error                 text NOT NULL DEFAULT '',
    stats                 jsonb NOT NULL DEFAULT '{}',
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ingresses_project ON ingresses (project_id, created_at DESC);
CREATE UNIQUE INDEX ingresses_stream_key ON ingresses (stream_key_hash) WHERE stream_key_hash IS NOT NULL;

CREATE TABLE servers (
    node_id      text PRIMARY KEY,
    region       text NOT NULL DEFAULT '',
    version      text NOT NULL DEFAULT '',
    address      text NOT NULL DEFAULT '',
    started_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    draining     boolean NOT NULL DEFAULT false,
    stats        jsonb NOT NULL DEFAULT '{}'
);

-- Room lifecycle and operational events: what the logs page shows.
CREATE TABLE events (
    id                   bigserial PRIMARY KEY,
    project_id           text,
    category             text NOT NULL,
    level                text NOT NULL DEFAULT 'info',
    type                 text NOT NULL,
    message              text NOT NULL DEFAULT '',
    room_name            text NOT NULL DEFAULT '',
    room_sid             text NOT NULL DEFAULT '',
    participant_identity text NOT NULL DEFAULT '',
    track_sid            text NOT NULL DEFAULT '',
    details              jsonb NOT NULL DEFAULT '{}',
    created_at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX events_project_time ON events (project_id, created_at DESC, id DESC);
CREATE INDEX events_project_category ON events (project_id, category, created_at DESC);
CREATE INDEX events_message_search ON events USING gin (to_tsvector('simple', message || ' ' || type || ' ' || room_name || ' ' || participant_identity));

CREATE TABLE audit_logs (
    id         bigserial PRIMARY KEY,
    org_id     uuid,
    project_id text,
    user_id    uuid,
    action     text NOT NULL,
    target     text NOT NULL DEFAULT '',
    ip         text NOT NULL DEFAULT '',
    details    jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_logs_project ON audit_logs (project_id, created_at DESC);

-- One row per project per minute, the source for charts and billing-style totals.
CREATE TABLE usage_metrics (
    project_id          text NOT NULL,
    bucket              timestamptz NOT NULL,
    participant_seconds bigint NOT NULL DEFAULT 0,
    room_seconds        bigint NOT NULL DEFAULT 0,
    audio_track_seconds bigint NOT NULL DEFAULT 0,
    video_track_seconds bigint NOT NULL DEFAULT 0,
    recording_seconds   bigint NOT NULL DEFAULT 0,
    bytes_in            bigint NOT NULL DEFAULT 0,
    bytes_out           bigint NOT NULL DEFAULT 0,
    packets_in          bigint NOT NULL DEFAULT 0,
    packets_out         bigint NOT NULL DEFAULT 0,
    packets_lost        bigint NOT NULL DEFAULT 0,
    connection_failures integer NOT NULL DEFAULT 0,
    peak_participants   integer NOT NULL DEFAULT 0,
    peak_rooms          integer NOT NULL DEFAULT 0,
    avg_rtt_ms          real NOT NULL DEFAULT 0,
    PRIMARY KEY (project_id, bucket)
);
