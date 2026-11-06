-- migrate:no-transaction
-- migrate:concurrent-index events_ingress
-- For GET /ingress/{id}/logs. events can be a large table, and building an index the ordinary way stops anything from
-- writing to it until that is done. A database that ran an earlier version of 0002 already has this index.
CREATE INDEX CONCURRENTLY IF NOT EXISTS events_ingress ON events ((details ->> 'ingress_id'), id DESC) WHERE details ? 'ingress_id';
