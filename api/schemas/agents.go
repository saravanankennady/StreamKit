package schemas

import "regexp"

// A key must not be able to smuggle something odd into a provider name that ends up in logs and audit entries.
var providerNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

const maxProviderSecretBytes = 8192

// SaveProviderCredential stores a provider's API key. The key is write-only: no response contains it.
type SaveProviderCredential struct {
	Provider string `json:"provider"`
	// Name tells apart several keys for one provider. Empty means "default".
	Name   string `json:"name"`
	Secret string `json:"secret"`
}

// Validate implements routes.Validator.
func (r *SaveProviderCredential) Validate() FieldErrors {
	errs := FieldErrors{}
	if !providerNamePattern.MatchString(r.Provider) {
		errs["provider"] = "must be a short lowercase name"
	}
	if r.Name != "" && !providerNamePattern.MatchString(r.Name) {
		errs["name"] = "must be a short lowercase name"
	}
	switch {
	case r.Secret == "":
		errs["secret"] = "is required"
	case len(r.Secret) > maxProviderSecretBytes:
		errs["secret"] = "is too long"
	}
	return errs
}

// ResolveProviderCredential asks for a key, from a worker.
type ResolveProviderCredential struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
}

// Validate implements routes.Validator.
func (r *ResolveProviderCredential) Validate() FieldErrors {
	errs := FieldErrors{}
	if !providerNamePattern.MatchString(r.Provider) {
		errs["provider"] = "must be a short lowercase name"
	}
	if r.Name != "" && !providerNamePattern.MatchString(r.Name) {
		errs["name"] = "must be a short lowercase name"
	}
	return errs
}

// AgentTurnReport is the part of a reported turn the server reads. The rest is stored as sent.
type AgentTurnReport struct {
	Interrupted bool `json:"interrupted"`
	Latency     struct {
		EndToEnd *float64 `json:"end_to_end"`
	} `json:"latency"`
}

// AgentSessionReport is the part of a worker's session report the server reads.
type AgentSessionReport struct {
	SessionID string            `json:"session_id"`
	Room      string            `json:"room"`
	JobID     string            `json:"job_id"`
	AgentName string            `json:"agent_name"`
	Pipeline  string            `json:"pipeline"`
	Models    map[string]string `json:"models"`
	StartedAt float64           `json:"started_at"`
	EndedAt   *float64          `json:"ended_at"`
	CostUSD   *float64          `json:"cost_usd"`
	Turns     []AgentTurnReport `json:"turns"`
	Errors    []map[string]any  `json:"errors"`
}

// Validate checks the report. Its body is otherwise free-form.
func (r *AgentSessionReport) Validate() FieldErrors {
	errs := FieldErrors{}
	checkName(errs, "session_id", r.SessionID, true, 64)
	checkName(errs, "room", r.Room, false, 256)
	checkName(errs, "job_id", r.JobID, false, 128)
	checkName(errs, "agent_name", r.AgentName, false, 256)
	checkName(errs, "pipeline", r.Pipeline, false, 64)
	if r.StartedAt <= 0 {
		errs["started_at"] = "is required"
	}
	return errs
}
