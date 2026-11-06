package room

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // the TURN REST credential format is defined with HMAC-SHA1
	"encoding/base64"
	"fmt"
	"time"

	"elnova-streamkit.local/pkg/pb"
	"elnova-streamkit.local/server/config"
)

// turnCredentialTTL is how long a minted TURN credential works. A client gets a new one each time it joins or
// resumes, so this only needs to cover the longest call.
const turnCredentialTTL = 24 * time.Hour

// mintTURN creates short-lived credentials in the format TURN servers such as coturn verify with a shared
// secret: the username holds the expiry, and the password is an HMAC of the username. A leaked credential
// stops working on its own, unlike a static password handed to every client.
func mintTURN(cfg config.TURN, identity string, now time.Time) *pb.ICEServer {
	username := fmt.Sprintf("%d:%s", now.Add(turnCredentialTTL).Unix(), identity)
	mac := hmac.New(sha1.New, []byte(cfg.Secret))
	mac.Write([]byte(username))
	return &pb.ICEServer{
		Urls:       cfg.URLs,
		Username:   username,
		Credential: base64.StdEncoding.EncodeToString(mac.Sum(nil)),
	}
}
