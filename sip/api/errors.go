package sipapi

import "encoding/json"

// jsonError writes the shape Twirp clients read. Go Twirp clients reject unknown keys, so this has only the keys they know.
func jsonError(e *Error) ([]byte, error) {
	body := struct {
		Code string            `json:"code"`
		Msg  string            `json:"msg"`
		Meta map[string]string `json:"meta,omitempty"`
	}{e.Code, e.Msg, e.Meta}
	return json.Marshal(body)
}
