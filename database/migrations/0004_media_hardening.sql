-- A URL input's address often has a password or a signed token in it, so it is kept the way stream outputs are: only a
-- redacted copy can be read, and workers open the sealed one.
ALTER TABLE ingresses ADD COLUMN source_url_sealed bytea;

-- What the API counts each time a recording is requested.
CREATE INDEX egresses_running ON egresses (project_id) WHERE status IN ('starting', 'active', 'ending');

-- Workers connect as this role instead of the owner of the database. They run beside media processes that parse input from
-- strangers, so what they can reach is kept to the job tables and what those need. The server gives it a password, and so
-- the right to log in, when STREAMKIT_WORKER_DB_PASSWORD is set. A role belongs to the whole cluster, so another database
-- may have created it already. A migration by a user that may not create roles leaves it out, and workers then have to
-- use whatever role they are given.
DO $$
BEGIN
    BEGIN
        CREATE ROLE streamkit_worker NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE;
    EXCEPTION
        WHEN duplicate_object OR unique_violation THEN NULL;
        WHEN insufficient_privilege THEN
            RAISE NOTICE 'not allowed to create the streamkit_worker role, so media workers need a role made by hand';
            RETURN;
    END;
    GRANT SELECT ON schema_migrations TO streamkit_worker;
    GRANT SELECT, UPDATE ON egresses, ingresses TO streamkit_worker;
    GRANT SELECT (id, deleted_at, status) ON projects TO streamkit_worker;
    GRANT SELECT, INSERT ON api_keys TO streamkit_worker;
    GRANT SELECT ON webhooks TO streamkit_worker;
    GRANT INSERT ON webhook_deliveries TO streamkit_worker;
    GRANT SELECT (id) ON webhook_deliveries TO streamkit_worker;
    GRANT INSERT ON events TO streamkit_worker;
    GRANT USAGE ON SEQUENCE events_id_seq TO streamkit_worker;
    GRANT SELECT, INSERT, UPDATE ON servers TO streamkit_worker;
END $$;
