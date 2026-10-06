package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mac-lucky/pushward-cli/internal/body"
	"github.com/mac-lucky/pushward-cli/internal/config"
	"github.com/mac-lucky/pushward-cli/internal/e2e"
)

var errNoE2EKey = &UsageError{errors.New("no encryption key: set " + config.EnvE2EKey + ", or run `pushward e2e generate --save` or `pushward e2e import`")}

// sealedFields are the notification fields an encryption key seals. The rest
// of the request (level, actions, metadata, target, ...) stays readable,
// since the server routes on it.
var sealedFields = []string{"title", "subtitle", "body", "url"}

// loadE2EKey returns the encryption key and where it came from, or nil when
// none is configured. A key that does not parse is an error rather than a
// reason to send in the clear.
func (a *App) loadE2EKey() (*e2e.Key, string, error) {
	raw, source := a.e2eKey, "the e2e-key input"
	if raw == "" {
		c, err := config.LoadE2E()
		if err != nil {
			return nil, "", err
		}
		a.warnOnce(c.Warning)
		raw, source = c.Key, c.Source
		if source == "env" {
			source = config.EnvE2EKey
		}
	}
	if raw == "" {
		return nil, "", nil
	}
	k, err := e2e.ParseKey(raw)
	if err != nil {
		return nil, "", usagef("encryption key from %s: %v", source, err)
	}
	return &k, source, nil
}

// messageFrom takes the sealed fields out of a request body.
func messageFrom(b map[string]any) (e2e.Message, error) {
	var m e2e.Message
	for i, dst := range []*string{&m.Title, &m.Subtitle, &m.Body, &m.URL} {
		v := b[sealedFields[i]]
		if v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return m, fmt.Errorf("%s must be a string to be encrypted", sealedFields[i])
		}
		*dst = s
	}
	return m, nil
}

// encryptBody replaces the text fields of a notification body with their
// envelope in encrypted, and returns the key ID it sealed with. Without a key
// it leaves the body alone and returns "", unless required. A body that
// already carries encrypted was sealed elsewhere and passes through.
func (a *App) encryptBody(b map[string]any, required bool) (string, error) {
	if v, ok := b["encrypted"]; ok {
		if s, _ := v.(string); s == "" {
			return "", usagef("encrypted must be a pw1 envelope (pushward e2e encrypt prints one)")
		}
		return "", nil
	}
	k, _, err := a.loadE2EKey()
	if err != nil {
		return "", err
	}
	if k == nil {
		if required {
			return "", errNoE2EKey
		}
		return "", nil
	}
	m, err := messageFrom(b)
	if err != nil {
		return "", usagef("%v", err)
	}
	env, err := k.Seal(m, a.Rand)
	if err != nil {
		return "", usagef("encrypting: %v", err)
	}
	for _, f := range sealedFields {
		delete(b, f)
	}
	b["encrypted"] = env
	return k.ID(), nil
}

func newE2ECmd(a *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "e2e",
		Short: "End-to-end encryption of notification text",
		Long: `With an encryption key, notify, notification send and schedule create
encrypt the title, subtitle, body and url before the request leaves this
machine. PushWard stores and forwards only the sealed text, and only devices
holding the key can read it. Level, sound, actions, metadata, thread, source
and target stay readable: the server needs them to deliver.

The key is read from PUSHWARD_E2E_KEY, then from the config file (e2e
generate --save or e2e import put it there). Create it here or in the
PushWard app (Settings, Encryption) and import it on the other side; e2e
key-id prints the Key ID to compare with the app. --no-encrypt sends one
notification without it, and pushward api never encrypts.`,
	}
	c.AddCommand(newE2EGenerateCmd(a), newE2EImportCmd(a), newE2EKeyIDCmd(a), newE2EEncryptCmd(a), newE2EDecryptCmd(a), newE2ERemoveCmd(a))
	return group(c)
}

// printKey prints a key the way generate shows it.
func (a *App) printKey(k e2e.Key, note string) error {
	data, err := json.Marshal(map[string]string{"key": k.Hex(), "key_id": k.ID()})
	if err != nil {
		return err
	}
	return a.out().Print(data, func(w io.Writer) error {
		fmt.Fprintf(w, "key     %s\nkey id  %s\n", k.Hex(), k.ID())
		if note != "" {
			fmt.Fprintln(w, note)
		}
		return nil
	})
}

// storeE2EKey writes the key into the config file and keeps the rest of it.
// A different key already there stays unless force is set: devices holding
// only that one could not read what is sent next.
func storeE2EKey(k e2e.Key, force bool) (string, error) {
	f, path, err := config.Read()
	if err != nil {
		return "", err
	}
	if f.E2EKey != "" && !force {
		old, err := e2e.ParseKey(f.E2EKey)
		if err != nil {
			return "", usagef("%s holds an encryption key that does not parse (%v); pass --force to replace it", path, err)
		}
		if old.ID() != k.ID() {
			return "", usagef("%s already holds encryption key %s; pass --force to replace it", path, old.ID())
		}
	}
	f.E2EKey = k.Hex()
	return config.Write(f)
}

func newE2EGenerateCmd(a *App) *cobra.Command {
	var save, force bool
	c := &cobra.Command{
		Use:   "generate",
		Short: "Create a new encryption key",
		Long: `Create a random 256-bit key and print it with its Key ID. Import it in the
PushWard app (Settings, Encryption) on the devices that should read the
notifications; iCloud Keychain carries it to your other devices. --save
also stores it in the config file, after which every notify is encrypted.

  pushward e2e generate --save`,
		Args: checkArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			k, err := e2e.Generate(a.Rand)
			if err != nil {
				return err
			}
			note := ""
			if save {
				path, err := storeE2EKey(k, force)
				if err != nil {
					return err
				}
				note = "stored in " + path
			}
			return a.printKey(k, note)
		},
	}
	c.Flags().BoolVar(&save, "save", false, "store the key in the config file")
	c.Flags().BoolVar(&force, "force", false, "with --save, replace a different key already stored")
	return c
}

func newE2EImportCmd(a *App) *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "import",
		Short: "Store an encryption key created in the app",
		Long: `Read an encryption key (64 hex characters; spaces and line breaks are
ignored) and store it in the config file (mode 0600). On a terminal it
prompts without echoing; otherwise it reads stdin.

  pbpaste | pushward e2e import`,
		Args: checkArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			var raw string
			if a.StdinTTY {
				s, err := a.readSecret("Paste your encryption key: ", false)
				if err != nil {
					return err
				}
				raw = s
			} else {
				data, _, err := body.Read("-", a.Stdin)
				if err != nil {
					return err
				}
				raw = string(data)
			}
			k, err := e2e.ParseKey(raw)
			if err != nil {
				return usagef("%v", err)
			}
			path, err := storeE2EKey(k, force)
			if err != nil {
				return err
			}
			data, err := json.Marshal(map[string]string{"key_id": k.ID(), "source": path})
			if err != nil {
				return err
			}
			if err := a.out().Printf(data, "stored encryption key %s in %s", k.ID(), path); err != nil {
				return err
			}
			if os.Getenv(config.EnvE2EKey) != "" {
				a.out().Warnf("%s is set and takes precedence over the stored key", config.EnvE2EKey)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "replace a different key already stored")
	return c
}

func newE2EKeyIDCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "key-id",
		Short: "Print the Key ID of the encryption key in use",
		Long: `Print the Key ID of the encryption key and where it comes from. The app
shows the Key ID of its keys too: when they match, both hold the same key.`,
		Args: checkArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			k, source, err := a.loadE2EKey()
			if err != nil {
				return err
			}
			if k == nil {
				return errNoE2EKey
			}
			data, err := json.Marshal(map[string]string{"key_id": k.ID(), "source": source})
			if err != nil {
				return err
			}
			return a.out().Printf(data, "%s (from %s)", k.ID(), source)
		},
	}
}

func newE2EEncryptCmd(a *App) *cobra.Command {
	var title, subtitle, text, link string
	c := &cobra.Command{
		Use:   "encrypt",
		Short: "Encrypt notification text and print the envelope",
		Long: `Seal a title, body and optional subtitle and url with the encryption key
and print {"encrypted":"pw1...."}: the field to send in place of those four
from a script or a tool that cannot encrypt. Without flags it reads a JSON
object with those keys from stdin.

  pushward e2e encrypt --title "Disk full" --body "/var at 97%" --jq .encrypted
  echo '{"title":"Disk full","body":"/var at 97%"}' | pushward e2e encrypt`,
		Args: checkArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b := map[string]any{"title": title, "subtitle": subtitle, "body": text, "url": link}
			if !anyChanged(cmd, "title", "subtitle", "body", "url") {
				if a.StdinTTY {
					return usagef("give --title and --body, or a JSON object on stdin")
				}
				data, _, err := body.Read("-", a.Stdin)
				if err != nil {
					return err
				}
				if err := json.Unmarshal(data, &b); err != nil {
					return usagef("stdin: want a JSON object with title and body: %v", err)
				}
			}
			k, _, err := a.loadE2EKey()
			if err != nil {
				return err
			}
			if k == nil {
				return errNoE2EKey
			}
			m, err := messageFrom(b)
			if err != nil {
				return usagef("%v", err)
			}
			env, err := k.Seal(m, a.Rand)
			if err != nil {
				return usagef("%v", err)
			}
			data, err := json.Marshal(map[string]string{"encrypted": env})
			if err != nil {
				return err
			}
			return a.out().Printf(data, "%s", env)
		},
	}
	c.Flags().StringVar(&title, "title", "", "title (required)")
	c.Flags().StringVar(&subtitle, "subtitle", "", "subtitle")
	c.Flags().StringVar(&text, "body", "", "body text (required)")
	c.Flags().StringVar(&link, "url", "", "URL opened when the notification is tapped")
	return c
}

func anyChanged(cmd *cobra.Command, names ...string) bool {
	for _, n := range names {
		if cmd.Flags().Changed(n) {
			return true
		}
	}
	return false
}

func newE2EDecryptCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "decrypt [envelope]",
		Short: "Decrypt an envelope and print its text",
		Long: `Open a pw1 envelope with the encryption key and print the title, subtitle,
body and url. The envelope is the argument, or stdin; stdin may also be
JSON with an encrypted field, such as the output of e2e encrypt or
schedule get.

  pushward schedule get 42 | pushward e2e decrypt`,
		Args: func(cmd *cobra.Command, got []string) error {
			if len(got) > 1 {
				return usagef("%s takes at most one envelope", cmd.CommandPath())
			}
			return nil
		},
		RunE: func(_ *cobra.Command, argv []string) error {
			var env string
			if len(argv) == 1 && argv[0] != "-" {
				env = argv[0]
			} else {
				data, _, err := body.Read("-", a.Stdin)
				if err != nil {
					return err
				}
				env = strings.TrimSpace(string(data))
				if strings.HasPrefix(env, "{") {
					var obj map[string]any
					if err := json.Unmarshal([]byte(env), &obj); err != nil {
						return usagef("stdin: invalid JSON: %v", err)
					}
					s, ok := obj["encrypted"].(string)
					if !ok {
						return usagef("stdin: the JSON has no encrypted field")
					}
					env = s
				}
			}
			k, _, err := a.loadE2EKey()
			if err != nil {
				return err
			}
			if k == nil {
				return errNoE2EKey
			}
			m, err := k.Open(env)
			if err != nil {
				return err
			}
			data, err := json.Marshal(m)
			if err != nil {
				return err
			}
			return a.out().Print(data, func(w io.Writer) error {
				for _, f := range [][2]string{{"title", m.Title}, {"subtitle", m.Subtitle}, {"body", m.Body}, {"url", m.URL}} {
					if f[1] != "" {
						fmt.Fprintf(w, "%-9s %s\n", f[0], f[1])
					}
				}
				return nil
			})
		},
	}
}

func newE2ERemoveCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "remove",
		Short: "Remove the stored encryption key",
		Long: `Remove the encryption key from the config file; notifications are sent
unencrypted from then on. Devices keep their copy.`,
		Args: checkArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			f, path, err := config.Read()
			if err != nil {
				return err
			}
			if f.E2EKey == "" {
				a.out().Human("no encryption key stored in %s", path)
				return nil
			}
			f.E2EKey = ""
			if _, err := config.Write(f); err != nil {
				return err
			}
			a.out().Human("removed the encryption key from %s", path)
			if os.Getenv(config.EnvE2EKey) != "" {
				a.out().Warnf("%s is still set in this shell", config.EnvE2EKey)
			}
			return nil
		},
	}
}
