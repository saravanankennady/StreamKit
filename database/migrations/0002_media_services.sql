-- Media services: the job queue for recording workers, and what ingest sessions need.
--
-- A job with status 'starting' and no worker is waiting in the queue. Workers claim jobs with FOR UPDATE SKIP
-- LOCKED, and hold them with a lease they renew about once a second. A worker that dies stops renewing, so
-- anyone can see the lease has run out and mark the job failed.
ALTER TABLE egresses
    ADD COLUMN worker_id          text,
    ADD COLUMN lease_expires_at   timestamptz,
    ADD COLUMN stop_requested_at  timestamptz,
    -- Stream URLs contain stream keys. Only redacted copies are readable: workers open the sealed ones.
    ADD COLUMN stream_urls        text[] NOT NULL DEFAULT '{}',
    ADD COLUMN stream_urls_sealed bytea,
    -- Which storage backend holds output_path, so a job outlives a change of configuration.
    ADD COLUMN storage            text NOT NULL DEFAULT '',
    ADD COLUMN cpu_seconds        double precision NOT NULL DEFAULT 0,
    ADD COLUMN updated_at         timestamptz NOT NULL DEFAULT now(),
    ADD CONSTRAINT egresses_status_known CHECK (status IN ('starting', 'active', 'ending', 'complete', 'failed', 'aborted'));
CREATE INDEX egresses_queue ON egresses (created_at) WHERE status = 'starting' AND worker_id IS NULL;
CREATE INDEX egresses_leases ON egresses (lease_expires_at) WHERE worker_id IS NOT NULL AND status IN ('starting', 'active', 'ending');
CREATE INDEX egresses_recordings ON egresses (project_id, created_at DESC, id DESC) WHERE format <> '';

ALTER TABLE ingresses
    ADD COLUMN worker_id         text,
    ADD COLUMN lease_expires_at  timestamptz,
    ADD COLUMN stop_requested_at timestamptz,
    -- A URL input has no publisher to wait for: this asks a worker to start pulling.
    ADD COLUMN pull_requested    boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT ingresses_status_known CHECK (status IN ('inactive', 'buffering', 'publishing', 'error'));
CREATE INDEX ingresses_pull_queue ON ingresses (created_at) WHERE pull_requested AND worker_id IS NULL;
CREATE INDEX ingresses_leases ON ingresses (lease_expires_at) WHERE worker_id IS NOT NULL;
