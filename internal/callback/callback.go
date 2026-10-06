// Package callback derives the secret that PushWard signs acknowledgement
// callbacks with (Standard Webhooks). testdata/callback-vectors-v1.json is
// the shared vector file the server signs against.
package callback

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
)

// Secret returns the whsec_ signing secret of an integration key. It comes
// from the key's SHA-256, the only form of the key the server keeps, so the
// server can sign without storing a secret and rolling the key changes it.
func Secret(integrationKey string) string {
	sum := sha256.Sum256([]byte(integrationKey))
	mac := hmac.New(sha256.New, sum[:])
	mac.Write([]byte("pushward/callback/v1"))
	return "whsec_" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
