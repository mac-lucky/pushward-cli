// Package e2e seals notification text for end-to-end encryption, envelope
// format pw1. The server stores and forwards the envelope; only devices that
// hold the key can open it. testdata/vectors-v1.json is the shared test
// vector file every implementation (server, apps, CLI, MCP, Home Assistant)
// must pass.
package e2e

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// MaxEnvelope is the longest envelope the API accepts, in characters.
	MaxEnvelope = 3072
	// MaxPlaintext is the most JSON that fits in MaxEnvelope once sealed.
	MaxPlaintext = 2266

	keySize   = 32
	nonceSize = 12
	tagSize   = 16
	padTo     = 64

	maxTitle = 256
	maxBody  = 4096
	maxURL   = 2048
)

// ErrIntegrationKey is returned for an hlk_ or hla_ value given where the
// encryption key belongs.
var ErrIntegrationKey = errors.New("that is an integration key, not an encryption key")

// Key is an encryption key with the AES key and key ID derived from it.
type Key struct {
	raw  []byte
	aead cipher.AEAD
	id   string
}

// derive computes the AES key and the key ID with HKDF-SHA256 and an empty
// salt, so every implementation gets them from the raw key alone.
func derive(raw []byte) (enc []byte, kid string, err error) {
	if enc, err = hkdf.Key(sha256.New, raw, nil, "pushward/e2e/v1/enc", 32); err != nil {
		return nil, "", err
	}
	id, err := hkdf.Key(sha256.New, raw, nil, "pushward/e2e/v1/kid", 4)
	if err != nil {
		return nil, "", err
	}
	return enc, hex.EncodeToString(id), nil
}

func newKey(raw []byte) (Key, error) {
	enc, kid, err := derive(raw)
	if err != nil {
		return Key{}, err
	}
	block, err := aes.NewCipher(enc)
	if err != nil {
		return Key{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return Key{}, err
	}
	return Key{raw: raw, aead: aead, id: kid}, nil
}

// Generate creates a random key. A nil reader means crypto/rand.
func Generate(random io.Reader) (Key, error) {
	if random == nil {
		random = rand.Reader
	}
	raw := make([]byte, keySize)
	if _, err := io.ReadFull(random, raw); err != nil {
		return Key{}, err
	}
	return newKey(raw)
}

// ParseKey reads a key as the apps show it: 64 hex characters, either case,
// with any spaces or line breaks ignored.
func ParseKey(s string) (Key, error) {
	h := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			return -1
		}
		return r
	}, s)
	if p := strings.ToLower(h); strings.HasPrefix(p, "hlk_") || strings.HasPrefix(p, "hla_") {
		return Key{}, ErrIntegrationKey
	}
	if len(h) != 2*keySize {
		return Key{}, fmt.Errorf("an encryption key is %d hex characters, got %d", 2*keySize, len(h))
	}
	raw, err := hex.DecodeString(h)
	if err != nil {
		return Key{}, fmt.Errorf("an encryption key is %d hex characters (0-9, a-f)", 2*keySize)
	}
	return newKey(raw)
}

// ID is the key ID, 8 hex characters. The apps show it as "Key ID" so both
// sides can check they hold the same key without showing it.
func (k Key) ID() string { return k.id }

// Hex is the key in the form ParseKey reads and the apps import.
func (k Key) Hex() string { return hex.EncodeToString(k.raw) }

// Message is the plaintext of an envelope.
type Message struct {
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
	Body     string `json:"body"`
	URL      string `json:"url,omitempty"`
}

// Validate applies the limits receivers enforce on what they open, so a
// sender hears about a long title or a bad url now rather than finding it cut
// or dropped on the phone.
func (m Message) Validate() error {
	switch {
	case m.Title == "":
		return errors.New("title is required")
	case m.Body == "":
		return errors.New("body is required")
	case utf8.RuneCountInString(m.Title) > maxTitle:
		return fmt.Errorf("title is longer than %d characters", maxTitle)
	case utf8.RuneCountInString(m.Subtitle) > maxTitle:
		return fmt.Errorf("subtitle is longer than %d characters", maxTitle)
	case utf8.RuneCountInString(m.Body) > maxBody:
		return fmt.Errorf("body is longer than %d characters", maxBody)
	}
	return checkURL(m.URL)
}

// checkURL is the server's url rule (model.ValidateActionURL), which
// receivers apply again to a url they decrypt.
func checkURL(s string) error {
	if s == "" {
		return nil
	}
	if utf8.RuneCountInString(s) > maxURL {
		return fmt.Errorf("url is longer than %d characters", maxURL)
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" {
		return errors.New("url must be a URL with a scheme")
	}
	switch strings.ToLower(u.Scheme) {
	case "javascript", "data", "file", "vbscript":
		return fmt.Errorf("url scheme %s is not allowed", u.Scheme)
	}
	if (u.Scheme == "http" || u.Scheme == "https") && u.Host == "" {
		return errors.New("url needs a host")
	}
	return nil
}

// envelopeLen is the length of the envelope for n bytes of plaintext.
func envelopeLen(n int) int {
	return len("pw1.") + 8 + 1 + base64.RawURLEncoding.EncodedLen(nonceSize+n+tagSize)
}

// Seal validates m and returns its envelope. The JSON is padded with spaces
// to a multiple of 64 bytes, so the length gives away less of the text,
// unless padding would push the envelope past MaxEnvelope. random supplies
// the nonce; nil means crypto/rand.
func (k Key) Seal(m Message, random io.Reader) (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return "", err
	}
	pt := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if r := len(pt) % padTo; r != 0 && envelopeLen(len(pt)+padTo-r) <= MaxEnvelope {
		pt = append(pt, bytes.Repeat([]byte(" "), padTo-r)...)
	}
	if len(pt) > MaxPlaintext {
		return "", fmt.Errorf("too long to encrypt: %d bytes of JSON, at most %d fit", len(pt), MaxPlaintext)
	}
	if random == nil {
		random = rand.Reader
	}
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(random, nonce); err != nil {
		return "", err
	}
	return k.seal(nonce, pt), nil
}

func (k Key) seal(nonce, plaintext []byte) string {
	sealed := k.aead.Seal(nonce, nonce, plaintext, []byte("pw1."+k.id))
	return "pw1." + k.id + "." + base64.RawURLEncoding.EncodeToString(sealed)
}

var envelopeShape = regexp.MustCompile(`^pw1\.([0-9a-f]{8})\.([A-Za-z0-9_-]{40,})$`)

// ParseEnvelope checks an envelope's form and returns its key ID and the
// sealed bytes (nonce, ciphertext, tag). The server runs the same checks.
func ParseEnvelope(s string) (kid string, sealed []byte, err error) {
	if s == "" || len(s) > MaxEnvelope {
		return "", nil, fmt.Errorf("an envelope is 1 to %d characters, got %d", MaxEnvelope, len(s))
	}
	m := envelopeShape.FindStringSubmatch(s)
	if m == nil {
		return "", nil, errors.New("not a pw1 envelope (pw1.<key id>.<base64url>)")
	}
	sealed, err = base64.RawURLEncoding.Strict().DecodeString(m[2])
	if err != nil {
		return "", nil, errors.New("envelope is not valid unpadded base64url")
	}
	if len(sealed) < nonceSize+2+tagSize {
		return "", nil, errors.New("envelope is too short")
	}
	return m[1], sealed, nil
}

// Open decrypts an envelope sealed with k. The text comes back the way the
// apps show it: title and subtitle cut to 256 characters, the body to 4096,
// and a url the server would refuse dropped.
func (k Key) Open(envelope string) (Message, error) {
	kid, sealed, err := ParseEnvelope(envelope)
	if err != nil {
		return Message{}, err
	}
	if kid != k.id {
		return Message{}, fmt.Errorf("sealed with key ID %s, not with this key (%s)", kid, k.id)
	}
	pt, err := k.aead.Open(nil, sealed[:nonceSize], sealed[nonceSize:], []byte("pw1."+kid))
	if err != nil {
		return Message{}, errors.New("cannot decrypt: the envelope was changed or sealed with another key")
	}
	m, err := decode(pt)
	if err != nil {
		return Message{}, err
	}
	m.Title = clamp(m.Title, maxTitle)
	m.Subtitle = clamp(m.Subtitle, maxTitle)
	m.Body = clamp(m.Body, maxBody)
	if checkURL(m.URL) != nil {
		m.URL = ""
	}
	return m, nil
}

// decode reads the plaintext strictly: an object with non-empty string title
// and body, and strings for subtitle and url when they are there. Other keys
// are ignored.
func decode(pt []byte) (Message, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(pt, &fields); err != nil || fields == nil {
		return Message{}, errors.New("decrypted text is not a JSON object")
	}
	get := func(name string, required bool) (string, error) {
		raw, ok := fields[name]
		if !ok {
			if required {
				return "", fmt.Errorf("decrypted text has no %s", name)
			}
			return "", nil
		}
		var s string
		if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
			return "", fmt.Errorf("decrypted %s is not a string", name)
		}
		if required && s == "" {
			return "", fmt.Errorf("decrypted %s is empty", name)
		}
		return s, nil
	}
	var m Message
	var err error
	if m.Title, err = get("title", true); err != nil {
		return Message{}, err
	}
	if m.Body, err = get("body", true); err != nil {
		return Message{}, err
	}
	if m.Subtitle, err = get("subtitle", false); err != nil {
		return Message{}, err
	}
	if m.URL, err = get("url", false); err != nil {
		return Message{}, err
	}
	return m, nil
}

// clamp cuts s to n Unicode code points.
func clamp(s string, n int) string {
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}
