-- A meeting's record button starts one job at a time. Other jobs in the room are the project's own, and limited per
-- project, because the API has always let a project run several. Only a job the button started has this set, so nothing
-- that was already running can stop this index from being built.
CREATE UNIQUE INDEX egresses_one_meeting_recording ON egresses (project_id, room_name)
    WHERE (request ->> 'meeting_recording') = 'true' AND status IN ('starting', 'active', 'ending');

-- Each join counts the rows a link has handed out, and starting a meeting counts the project's meetings with working links.
CREATE INDEX meeting_members_link ON meeting_members (link_id);
CREATE INDEX join_links_room ON join_links (project_id, room);

-- For purging old rows. See PurgeOld. Members go by when they were last active now, not when they first came.
DROP INDEX meeting_members_created;
CREATE INDEX meeting_members_seen ON meeting_members (seen_at);
CREATE INDEX audit_logs_created ON audit_logs (created_at);
CREATE INDEX join_links_ended ON join_links ((COALESCE(revoked_at, expires_at)));
CREATE INDEX meetings_created ON meetings (created_at);
