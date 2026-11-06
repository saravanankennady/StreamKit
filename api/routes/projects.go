package routes

import (
	"context"
	"errors"
	"net/http"
	"time"

	"elnova-streamkit.local/api/schemas"
	"elnova-streamkit.local/api/store"
	"elnova-streamkit.local/server/auth"
)

// Project routes name their project in the path, so they check membership themselves.
func (s *Server) projectForUser(r *http.Request, c *reqCtx, adminRequired bool) (*store.Project, error) {
	project, err := s.store.ProjectByID(r.Context(), r.PathValue("id"))
	if err != nil {
		return nil, errNotFound
	}
	role, err := s.store.RoleIn(r.Context(), c.user.ID, project.OrgID)
	if err != nil {
		return nil, errNotFound
	}
	if adminRequired && role == "member" {
		return nil, errForbidden
	}
	c.project = project
	return project, nil
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	projects, err := s.store.ProjectsForUser(r.Context(), c.user.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": s.projectViews(projects)})
	return nil
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var in schemas.CreateProject
	if err := decode(w, r, &in); err != nil {
		return err
	}
	orgID := in.OrgID
	if orgID == "" {
		orgs, err := s.store.OrganizationsOf(r.Context(), c.user.ID)
		if err != nil {
			return err
		}
		if len(orgs) == 0 {
			return errorf(http.StatusBadRequest, "no_organization", "You are not in any organization.")
		}
		orgID = orgs[0].ID
	}
	role, err := s.store.RoleIn(r.Context(), c.user.ID, orgID)
	if err != nil {
		return errNotFound
	}
	if role == "member" {
		return errForbidden
	}
	region := in.Region
	if region == "" {
		region = "default"
	}
	env := in.Environment
	if env == "" {
		env = "development"
	}
	settings := store.ProjectSettings{}
	if in.Settings != nil {
		settings = *in.Settings
	}
	project, err := s.store.CreateProject(r.Context(), orgID, in.Name, region, env, settings)
	if err != nil {
		return err
	}
	c.project = project
	s.audit(r, c, "project.created", project.Name, nil)
	writeJSON(w, http.StatusCreated, projectView{project, s.publicURL()})
	return nil
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	project, err := s.projectForUser(r, c, false)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, projectView{project, s.publicURL()})
	return nil
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	project, err := s.projectForUser(r, c, true)
	if err != nil {
		return err
	}
	var in schemas.UpdateProject
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if in.Name != nil {
		project.Name = *in.Name
	}
	if in.Region != nil {
		project.Region = *in.Region
	}
	if in.Environment != nil {
		project.Environment = *in.Environment
	}
	if in.Status != nil {
		project.Status = *in.Status
	}
	if in.Settings != nil {
		project.Settings = *in.Settings
	}
	updated, err := s.store.UpdateProject(r.Context(), project)
	if err != nil {
		return err
	}
	s.invalidateProject(updated.ID)
	// Suspending takes effect for new connections at once.
	s.keys.InvalidateAll()
	if updated.Status != "active" {
		if err := s.store.SuspendProjectMedia(r.Context(), updated.ID); err != nil {
			s.log.Warn("could not stop a suspended project's media", "project", updated.ID, "error", err)
		}
	}
	s.audit(r, c, "project.updated", updated.Name, in)
	writeJSON(w, http.StatusOK, projectView{updated, s.publicURL()})
	return nil
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	project, err := s.projectForUser(r, c, true)
	if err != nil {
		return err
	}
	if err := s.store.DeleteProject(r.Context(), project.ID); err != nil {
		return err
	}
	s.keys.InvalidateAll()
	s.invalidateProject(project.ID)
	for _, x := range s.rooms.List(project.ID, nil) {
		_ = s.rooms.Delete(project.ID, x.Name())
	}
	s.audit(r, c, "project.deleted", project.Name, nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	keys, err := s.store.ListAPIKeys(r.Context(), c.project.ID)
	if err != nil {
		return err
	}
	if keys == nil {
		keys = []store.APIKey{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": keys})
	return nil
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var in schemas.CreateKey
	if err := decode(w, r, &in); err != nil {
		return err
	}
	key, err := s.store.CreateAPIKey(r.Context(), c.project.ID, in.Name, nil)
	if err != nil {
		return err
	}
	s.audit(r, c, "api_key.created", key.APIKey, nil)
	writeJSON(w, http.StatusCreated, key)
	return nil
}

func (s *Server) rotateKey(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var in schemas.RotateKey
	if err := decode(w, r, &in); err != nil {
		return err
	}
	overlap := 24 * time.Hour
	if in.OverlapSeconds > 0 {
		overlap = time.Duration(in.OverlapSeconds) * time.Second
	}
	key, err := s.store.RotateAPIKey(r.Context(), c.project.ID, r.PathValue("id"), overlap)
	if err != nil {
		return err
	}
	s.keys.InvalidateAll()
	s.audit(r, c, "api_key.rotated", key.APIKey, map[string]any{"replaced": r.PathValue("id"), "overlap_seconds": int(overlap.Seconds())})
	writeJSON(w, http.StatusCreated, key)
	return nil
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := s.store.RevokeAPIKey(r.Context(), c.project.ID, r.PathValue("id")); err != nil {
		return err
	}
	s.keys.InvalidateAll()
	s.audit(r, c, "api_key.revoked", r.PathValue("id"), nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

var errNoAPIKey = errorf(http.StatusConflict, "no_api_key", "Create an API key for this project first.")

// signingKey returns the project's newest usable API key.
func (s *Server) signingKey(ctx context.Context, projectID string) (auth.Key, error) {
	keys, err := s.store.ListAPIKeys(ctx, projectID)
	if err != nil {
		return auth.Key{}, err
	}
	for _, k := range keys {
		if k.RevokedAt != nil || (k.ExpiresAt != nil && k.ExpiresAt.Before(time.Now())) {
			continue
		}
		key, err := s.store.KeyByID(ctx, projectID, k.ID)
		if err == nil {
			return key, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return auth.Key{}, err
		}
	}
	return auth.Key{}, errNoAPIKey
}

// createToken mints a token, for project members only. An access token cannot mint more tokens: a leaked one would
// otherwise be good forever.
func (s *Server) createToken(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var in schemas.CreateToken
	if err := decode(w, r, &in); err != nil {
		return err
	}
	key, err := s.signingKey(r.Context(), c.project.ID)
	if err != nil {
		return err
	}
	ttl := auth.DefaultTokenTTL
	if c.project.Settings.TokenTTLSeconds > 0 {
		ttl = time.Duration(c.project.Settings.TokenTTLSeconds) * time.Second
	}
	if in.TTLSeconds > 0 {
		ttl = time.Duration(in.TTLSeconds) * time.Second
	}
	token, err := auth.Mint(key, &auth.Grants{
		Identity:   in.Identity,
		Name:       in.Name,
		Metadata:   in.Metadata,
		Attributes: in.Attributes,
		Video: &auth.VideoGrant{
			RoomJoin: true, Room: in.Room,
			RoomAdmin: in.RoomAdmin, RoomCreate: in.RoomCreate, RoomList: in.RoomList, RoomRecord: in.RoomRecord,
			CanPublish: in.CanPublish, CanSubscribe: in.CanSubscribe, CanPublishData: in.CanPublishData,
			CanPublishSources: in.CanPublishSources, CanUpdateOwnMetadata: in.CanUpdateOwnMetadata, Hidden: in.Hidden,
		},
	}, ttl)
	if err != nil {
		return err
	}
	s.audit(r, c, "token.created", in.Identity, map[string]any{"room": in.Room, "ttl_seconds": int(ttl.Seconds()), "room_admin": in.RoomAdmin})
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "url": s.publicURL(), "expires_at": time.Now().Add(ttl)})
	return nil
}
