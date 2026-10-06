package callback

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestSecretVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/callback-vectors-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Cases []struct {
			IntegrationKey   string `json:"integration_key"`
			KeyHashHex       string `json:"key_hash_hex"`
			SecretHex        string `json:"secret_hex"`
			WHSec            string `json:"whsec"`
			WebhookID        string `json:"webhook_id"`
			WebhookTimestamp string `json:"webhook_timestamp"`
			Body             string `json:"body"`
			Signature        string `json:"webhook_signature"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range v.Cases {
		if sum := sha256.Sum256([]byte(c.IntegrationKey)); hex.EncodeToString(sum[:]) != c.KeyHashHex {
			t.Errorf("%s: key hash %x", c.IntegrationKey, sum)
		}
		got := Secret(c.IntegrationKey)
		if got != c.WHSec {
			t.Errorf("%s: secret %s, want %s", c.IntegrationKey, got, c.WHSec)
		}
		// The printed secret is what a receiver verifies with.
		secret, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(got, "whsec_"))
		if err != nil || hex.EncodeToString(secret) != c.SecretHex {
			t.Fatalf("%s: secret bytes %x %v", c.IntegrationKey, secret, err)
		}
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte(c.WebhookID + "." + c.WebhookTimestamp + "." + c.Body))
		if sig := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)); sig != c.Signature {
			t.Errorf("%s: signature %s, want %s", c.IntegrationKey, sig, c.Signature)
		}
	}
}
