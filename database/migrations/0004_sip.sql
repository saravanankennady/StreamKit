-- SIP: trunks, dispatch rules, calls and transfers.
--
-- A trunk's description is stored as JSON in the shape of its API message, without its credentials. Those, and the PIN of a
-- dispatch rule, are only ever stored sealed with the master key. Nothing reads them back out through an API.
CREATE TABLE sip_trunks (
    id          text PRIMARY KEY,
    project_id  text NOT NULL,
    direction   text NOT NULL CHECK (direction IN ('inbound', 'outbound')),
    info        jsonb NOT NULL,
    -- The numbers this trunk answers for, in normal form, so an INVITE finds its trunks without reading every one.
    numbers     text[] NOT NULL DEFAULT '{}',
    auth_sealed bytea,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sip_trunks_project ON sip_trunks (project_id, direction, created_at);
CREATE INDEX sip_trunks_numbers ON sip_trunks USING gin (numbers) WHERE direction = 'inbound';

CREATE TABLE sip_dispatch_rules (
    id         text PRIMARY KEY,
    project_id text NOT NULL,
    info       jsonb NOT NULL,
    pin_sealed bytea,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sip_dispatch_rules_project ON sip_dispatch_rules (project_id, created_at);

-- One row per call, in either direction. An outbound call with status 'queued' and no worker is waiting in the queue, the same
-- way a recording job does. Workers hold what they run with a lease, so a worker that dies leaves calls anyone can see are dead.
CREATE TABLE sip_calls (
    id                   text PRIMARY KEY,
    project_id           text NOT NULL,
    direction            text NOT NULL CHECK (direction IN ('inbound', 'outbound')),
    status               text NOT NULL CHECK (status IN ('queued', 'dialing', 'ringing', 'active', 'ended', 'failed')),
    trunk_id             text NOT NULL DEFAULT '',
    dispatch_rule_id     text NOT NULL DEFAULT '',
    room_name            text NOT NULL DEFAULT '',
    room_sid             text NOT NULL DEFAULT '',
    participant_identity text NOT NULL DEFAULT '',
    participant_sid      text NOT NULL DEFAULT '',
    -- Empty for a call whose rule or request hides the phone number.
    from_user            text NOT NULL DEFAULT '',
    from_host            text NOT NULL DEFAULT '',
    to_user              text NOT NULL DEFAULT '',
    to_host              text NOT NULL DEFAULT '',
    sip_call_id          text NOT NULL DEFAULT '',
    audio_codec          text NOT NULL DEFAULT '',
    media_encryption     text NOT NULL DEFAULT '',
    -- What an outbound call was asked to do. It can hold a trunk's credentials, so it is sealed.
    request_sealed       bytea,
    worker_id            text,
    lease_expires_at     timestamptz,
    stop_requested_at    timestamptz,
    sip_status_code      integer NOT NULL DEFAULT 0,
    sip_status           text NOT NULL DEFAULT '',
    disconnect_reason    text NOT NULL DEFAULT '',
    error                text NOT NULL DEFAULT '',
    created_at           timestamptz NOT NULL DEFAULT now(),
    answered_at          timestamptz,
    ended_at             timestamptz,
    updated_at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sip_calls_queue ON sip_calls (created_at) WHERE status = 'queued' AND worker_id IS NULL;
CREATE INDEX sip_calls_leases ON sip_calls (worker_id, lease_expires_at) WHERE worker_id IS NOT NULL AND status IN ('dialing', 'ringing', 'active');
CREATE INDEX sip_calls_open ON sip_calls (project_id) WHERE status IN ('queued', 'dialing', 'ringing', 'active');
CREATE INDEX sip_calls_room ON sip_calls (project_id, room_name, participant_identity) WHERE status IN ('queued', 'dialing', 'ringing', 'active');
CREATE INDEX sip_calls_history ON sip_calls (project_id, created_at DESC, id DESC);

CREATE TABLE sip_transfers (
    id              text PRIMARY KEY,
    call_id         text NOT NULL REFERENCES sip_calls ON DELETE CASCADE,
    project_id      text NOT NULL,
    status          text NOT NULL CHECK (status IN ('pending', 'ongoing', 'successful', 'failed')),
    transfer_to     text NOT NULL,
    request         jsonb NOT NULL DEFAULT '{}',
    reason          text NOT NULL DEFAULT '',
    sip_status_code integer NOT NULL DEFAULT 0,
    sip_status      text NOT NULL DEFAULT '',
    error           text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sip_transfers_call ON sip_transfers (call_id, created_at);
CREATE INDEX sip_transfers_pending ON sip_transfers (call_id) WHERE status = 'pending';
