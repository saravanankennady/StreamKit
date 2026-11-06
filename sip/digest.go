package sip

import (
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // SIP digest authentication is defined over MD5, and carriers use it.
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/icholy/digest"
)

const (
	// nonceLife is how long a challenge can be answered. A phone answers in milliseconds, and a slow carrier in seconds.
	nonceLife = 5 * time.Minute
	// defaultRealm is used when a trunk does not name one.
	defaultRealm = "StreamKit"
	// usedDigestLimit bounds how many answers this worker remembers.
	usedDigestLimit = 20000
)

// digestAuth checks INVITEs against trunk credentials. It keeps nothing for a challenge it sends: the nonce is signed and carries its
// own time, so a flood of INVITEs that never answer cost no memory, and any worker can check an answer to another's challenge.
type digestAuth struct {
	key []byte

	mu   sync.Mutex
	used map[string]time.Time
}

// newDigestAuth derives the signing key from the master key, so all workers agree, and it is not the master key itself.
func newDigestAuth(masterKey string) *digestAuth {
	sum := sha256.Sum256([]byte("streamkit sip digest nonce\x00" + masterKey))
	return &digestAuth{key: sum[:], used: map[string]time.Time{}}
}

func (d *digestAuth) sign(when int64, callID, trunkID string) []byte {
	mac := hmac.New(sha256.New, d.key)
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(when))
	mac.Write(ts[:])
	mac.Write([]byte{0})
	mac.Write([]byte(callID))
	mac.Write([]byte{0})
	mac.Write([]byte(trunkID))
	return append(ts[:], mac.Sum(nil)[:16]...)
}

// nonce makes a value bound to this call and trunk, so an answer can only be used for what it was asked about.
func (d *digestAuth) nonce(now time.Time, callID, trunkID string) string {
	return base64.RawURLEncoding.EncodeToString(d.sign(now.Unix(), callID, trunkID))
}

type nonceState int

const (
	nonceBad nonceState = iota
	nonceStale
	nonceGood
)

func (d *digestAuth) checkNonce(now time.Time, nonce, callID, trunkID string) nonceState {
	raw, err := base64.RawURLEncoding.DecodeString(nonce)
	if err != nil || len(raw) != 24 {
		return nonceBad
	}
	when := int64(binary.BigEndian.Uint64(raw[:8]))
	if !hmac.Equal(raw, d.sign(when, callID, trunkID)) {
		return nonceBad
	}
	if age := now.Sub(time.Unix(when, 0)); age < -time.Minute || age > nonceLife {
		return nonceStale
	}
	return nonceGood
}

// challenge makes the value of a Proxy-Authenticate header.
func (d *digestAuth) challenge(now time.Time, realm, callID, trunkID string, stale bool) string {
	c := digest.Challenge{Realm: realm, Nonce: d.nonce(now, callID, trunkID), Algorithm: "MD5", QOP: []string{"auth"}, Stale: stale}
	return c.String()
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s)) //nolint:gosec // See the import.
	return hex.EncodeToString(sum[:])
}

// digestResponse works out what the answer should be.
func digestResponse(c *digest.Credentials, password, method string) string {
	ha1 := md5Hex(c.Username + ":" + c.Realm + ":" + password)
	ha2 := md5Hex(method + ":" + c.URI)
	if c.QOP == "" {
		return md5Hex(ha1 + ":" + c.Nonce + ":" + ha2)
	}
	return md5Hex(fmt.Sprintf("%s:%s:%08x:%s:%s:%s", ha1, c.Nonce, c.Nc, c.Cnonce, c.QOP, ha2))
}

type authResult int

const (
	authChallenge authResult = iota // There was no answer yet, so ask.
	authStale                       // The answer was fine, but too old. Ask again.
	authBad                         // The answer was wrong.
	authOK
)

// check reads a Proxy-Authorization header. A correct answer is only good once: the transaction layer has already absorbed real
// retransmissions, so seeing the same answer on a new transaction is a replay.
func (d *digestAuth) check(now time.Time, header, method, realm, username, password, callID, trunkID string) authResult {
	if header == "" {
		return authChallenge
	}
	c, err := digest.ParseCredentials(header)
	if err != nil || c.Nonce == "" {
		return authBad
	}
	if alg := strings.ToUpper(c.Algorithm); alg != "" && alg != "MD5" {
		return authBad
	}
	if c.QOP != "" && c.QOP != "auth" {
		return authBad
	}
	if c.Realm != realm {
		return authBad
	}
	switch d.checkNonce(now, c.Nonce, callID, trunkID) {
	case nonceBad:
		return authBad
	case nonceStale:
		return authStale
	}
	// Both are compared whatever the first said, so the time this takes says nothing about which was wrong.
	userOK := subtle.ConstantTimeCompare([]byte(c.Username), []byte(username))
	responseOK := subtle.ConstantTimeCompare([]byte(strings.ToLower(c.Response)), []byte(digestResponse(c, password, method)))
	if userOK&responseOK != 1 {
		return authBad
	}
	if !d.firstUse(now, c.Nonce+"\x00"+c.Response+"\x00"+c.Cnonce) {
		return authStale
	}
	return authOK
}

func (d *digestAuth) firstUse(now time.Time, key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, seen := d.used[key]; seen {
		return false
	}
	if len(d.used) >= usedDigestLimit {
		for k, at := range d.used {
			if now.Sub(at) > nonceLife {
				delete(d.used, k)
			}
		}
		if len(d.used) >= usedDigestLimit {
			// Everything is recent, which takes a very busy or very strange worker. Forgetting beats refusing real callers.
			d.used = map[string]time.Time{}
		}
	}
	d.used[key] = now
	return true
}
