package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// keyCacheFor is how long a key that passed the check is taken as personal
// without asking the server again.
const keyCacheFor = time.Hour

// KeyCache remembers across hook processes that a key passed the check, so
// a prompt does not start with a create the server is meant to refuse. Each
// entry is the SHA-256 of the API URL and the key, never the key, with the
// time of the check. A refusal is not remembered: the next hook asks again.
type KeyCache struct {
	// Path is the file, 0600.
	Path string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	id  string
}

// NewKeyCache is the cache entry of one key against one API.
func NewKeyCache(path, apiURL, key string) *KeyCache {
	sum := sha256.Sum256([]byte(apiURL + "\n" + key))
	return &KeyCache{Path: path, id: hex.EncodeToString(sum[:])}
}

func (c *KeyCache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// fresh reports whether at is within the last keyCacheFor. A time ahead of
// the clock (it was set back) is not.
func (c *KeyCache) fresh(at int64, now time.Time) bool {
	t := time.Unix(at, 0)
	return !t.After(now) && now.Sub(t) < keyCacheFor
}

// read is the file's entries; a missing or unreadable file has none.
func (c *KeyCache) read() map[string]int64 {
	entries := map[string]int64{}
	data, err := os.ReadFile(c.Path) // #nosec G304 -- the CLI's own state file
	if err == nil {
		_ = json.Unmarshal(data, &entries)
	}
	return entries
}

// passed reports whether this key passed the check in the last hour.
func (c *KeyCache) passed() bool {
	at, ok := c.read()[c.id]
	return ok && c.fresh(at, c.now())
}

// save records that this key passed the check now, and drops the entries
// that are no longer fresh.
func (c *KeyCache) save() error {
	now := c.now()
	entries := c.read()
	for id, at := range entries {
		if !c.fresh(at, now) {
			delete(entries, id)
		}
	}
	entries[c.id] = now.Unix()
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o700); err != nil {
		return err
	}
	return writeFile(c.Path, append(data, '\n'), 0o600)
}
