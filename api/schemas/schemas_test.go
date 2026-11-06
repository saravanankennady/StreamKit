package schemas

import (
	"strings"
	"testing"

	"elnova-streamkit.local/api/store"
)

func TestRegister(t *testing.T) {
	good := Register{Email: "a@example.com", Password: "long-enough-pw", Name: "A", Organization: "O"}
	if errs := good.Validate(); len(errs) != 0 {
		t.Fatalf("valid: %v", errs)
	}
	cases := []struct {
		field string
		edit  func(*Register)
	}{
		{"email", func(r *Register) { r.Email = "not-an-email" }},
		{"email", func(r *Register) { r.Email = "Name <a@example.com>" }},
		{"password", func(r *Register) { r.Password = "short" }},
		{"password", func(r *Register) { r.Password = strings.Repeat("x", 300) }},
		{"name", func(r *Register) { r.Name = "   " }},
		{"organization", func(r *Register) { r.Organization = "" }},
	}
	for _, c := range cases {
		r := good
		c.edit(&r)
		if errs := r.Validate(); errs[c.field] == "" {
			t.Errorf("%+v should fail on %s: %v", r, c.field, errs)
		}
	}
}

func TestProjectSettings(t *testing.T) {
	bad := []store.ProjectSettings{
		{AllowedOrigins: []string{"not a url"}},
		{AllowedOrigins: []string{"https://a.example.com/path"}},
		{ICEServers: []store.ICEServer{{}}},
		{ICEServers: []store.ICEServer{{URLs: []string{"http://x"}}}},
		{TokenTTLSeconds: -1},
	}
	for _, s := range bad {
		r := CreateProject{Name: "p", Settings: &s}
		if len(r.Validate()) == 0 {
			t.Errorf("%+v should be invalid", s)
		}
	}
	good := store.ProjectSettings{
		AllowedOrigins: []string{"https://a.example.com", "*"},
		ICEServers:     []store.ICEServer{{URLs: []string{"turn:t.example.com:3478"}, Username: "u", Credential: "c"}},
	}
	r := CreateProject{Name: "p", Environment: "production", Settings: &good}
	if errs := r.Validate(); len(errs) != 0 {
		t.Fatal(errs)
	}
	r.Environment = "nope"
	if r.Validate()["environment"] == "" {
		t.Fatal("bad environment accepted")
	}
}

func TestWebhookValidation(t *testing.T) {
	good := Webhook{URL: "https://hooks.example.com/x", APIKeyID: "id", Events: []string{"room_started"}}
	if errs := good.Validate(); len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, bad := range []Webhook{
		{URL: "ftp://x", APIKeyID: "id"},
		{URL: "not a url", APIKeyID: "id"},
		{URL: "https://x.example.com"},
		{URL: "https://x.example.com", APIKeyID: "id", Events: []string{"made_up"}},
	} {
		if len(bad.Validate()) == 0 {
			t.Errorf("%+v should be invalid", bad)
		}
	}
}
