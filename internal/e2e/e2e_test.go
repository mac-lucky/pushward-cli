package e2e

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

type vectorFile struct {
	Seal []struct {
		Name      string  `json:"name"`
		KeyHex    string  `json:"key_hex"`
		EncKeyHex string  `json:"enc_key_hex"`
		KID       string  `json:"kid"`
		NonceHex  string  `json:"nonce_hex"`
		Plaintext string  `json:"plaintext"`
		Envelope  string  `json:"envelope"`
		Expect    Message `json:"expect"`
	} `json:"seal"`
	OpenFail []struct {
		Name     string `json:"name"`
		KeyHex   string `json:"key_hex"`
		Envelope string `json:"envelope"`
	} `json:"open_fail"`
	ParseFail []struct {
		Name     string `json:"name"`
		Envelope string `json:"envelope"`
	} `json:"parse_fail"`
}

// vectors loads the shared vector file. It is the published copy, so it is
// read as is and never regenerated here.
func vectors(t *testing.T) vectorFile {
	t.Helper()
	data, err := os.ReadFile("testdata/vectors-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Seal) == 0 || len(v.OpenFail) == 0 || len(v.ParseFail) == 0 {
		t.Fatal("vector file has an empty section")
	}
	return v
}

func mustKey(t *testing.T, h string) Key {
	t.Helper()
	k, err := ParseKey(h)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func mustHex(t *testing.T, h string) []byte {
	t.Helper()
	b, err := hex.DecodeString(h)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVectorsSeal(t *testing.T) {
	for _, c := range vectors(t).Seal {
		t.Run(c.Name, func(t *testing.T) {
			k := mustKey(t, c.KeyHex)
			enc, kid, err := derive(mustHex(t, c.KeyHex))
			if err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(enc) != c.EncKeyHex || kid != c.KID || k.ID() != c.KID {
				t.Errorf("derived enc %x kid %s (key %s), want %s %s", enc, kid, k.ID(), c.EncKeyHex, c.KID)
			}
			if got := k.seal(mustHex(t, c.NonceHex), []byte(c.Plaintext)); got != c.Envelope {
				t.Errorf("seal\n got  %s\n want %s", got, c.Envelope)
			}
			m, err := k.Open(c.Envelope)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if m != c.Expect {
				t.Errorf("open\n got  %+v\n want %+v", m, c.Expect)
			}
		})
	}
}

func TestVectorsOpenFail(t *testing.T) {
	for _, c := range vectors(t).OpenFail {
		if m, err := mustKey(t, c.KeyHex).Open(c.Envelope); err == nil {
			t.Errorf("%s: opened to %+v", c.Name, m)
		}
	}
}

func TestVectorsParseFail(t *testing.T) {
	k := mustKey(t, strings.Repeat("00", keySize))
	for _, c := range vectors(t).ParseFail {
		if _, _, err := ParseEnvelope(c.Envelope); err == nil {
			t.Errorf("%s: parsed", c.Name)
		}
		if _, err := k.Open(c.Envelope); err == nil {
			t.Errorf("%s: opened", c.Name)
		}
	}
}

// Seal pads to 64 bytes, except when that would not fit: the padded and the
// maximum-size vectors are exactly what Seal produces for their text.
func TestSealMatchesVectors(t *testing.T) {
	for _, c := range vectors(t).Seal {
		if c.Name != "padded_to_64" && c.Name != "max_size" {
			continue
		}
		k := mustKey(t, c.KeyHex)
		got, err := k.Seal(c.Expect, bytes.NewReader(mustHex(t, c.NonceHex)))
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if got != c.Envelope {
			t.Errorf("%s:\n got  %s\n want %s", c.Name, got, c.Envelope)
		}
	}
}

func TestSealPadsAndRoundTrips(t *testing.T) {
	k, err := Generate(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []Message{
		{Title: "a", Body: "b"},
		{Title: "Deploy failed", Subtitle: "api", Body: "Step <3> & more", URL: "https://ci.example.com/runs/42"},
		{Title: "Za\u017c\u00f3\u0142\u0107", Body: strings.Repeat("\u0142", 1000)},
	} {
		env, err := k.Seal(m, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, sealed, err := ParseEnvelope(env)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(sealed) - nonceSize - tagSize; n%padTo != 0 {
			t.Errorf("%q: plaintext of %d bytes is not padded", m.Title, n)
		}
		got, err := k.Open(env)
		if err != nil || got != m {
			t.Errorf("round trip: %+v %v, want %+v", got, err, m)
		}
	}
}

func TestSealRefuses(t *testing.T) {
	k := mustKey(t, strings.Repeat("ab", keySize))
	for name, m := range map[string]Message{
		"no title":       {Body: "b"},
		"no body":        {Title: "t"},
		"long title":     {Title: strings.Repeat("\u00e9", 257), Body: "b"},
		"long subtitle":  {Title: "t", Subtitle: strings.Repeat("x", 257), Body: "b"},
		"long body":      {Title: "t", Body: strings.Repeat("x", 4097)},
		"javascript url": {Title: "t", Body: "b", URL: "JavaScript:alert(1)"},
		"no scheme":      {Title: "t", Body: "b", URL: "example.com/x"},
		"http no host":   {Title: "t", Body: "b", URL: "https:///x"},
		"too long":       {Title: "t", Body: strings.Repeat("x", MaxPlaintext)},
	} {
		if env, err := k.Seal(m, nil); err == nil {
			t.Errorf("%s: sealed %s", name, env)
		}
	}
	if _, err := k.Seal(Message{Title: "t", Body: "b", URL: "myapp://x"}, nil); err != nil {
		t.Errorf("custom scheme refused: %v", err)
	}
}

func TestEnvelopeLimits(t *testing.T) {
	if envelopeLen(MaxPlaintext) != MaxEnvelope || envelopeLen(MaxPlaintext+1) <= MaxEnvelope {
		t.Errorf("MaxPlaintext %d gives %d characters", MaxPlaintext, envelopeLen(MaxPlaintext))
	}
}

func TestParseKey(t *testing.T) {
	want := mustKey(t, "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	for _, in := range []string{
		"000102030405060708090A0B0C0D0E0F101112131415161718191A1B1C1D1E1F",
		"  0001 0203 0405 0607 0809 0a0b 0c0d 0e0f\n1011 1213 1415 1617 1819 1a1b 1c1d 1e1f\n",
		"\t000102030405060708090a0b0c0d0e0f\r\n101112131415161718191a1b1c1d1e1f",
	} {
		k, err := ParseKey(in)
		if err != nil || k.ID() != want.ID() || k.Hex() != want.Hex() {
			t.Errorf("%q: %v %s", in, err, k.ID())
		}
	}
	for _, in := range []string{"hlk_0123456789abcdef", " HLA_x", "abc", strings.Repeat("g", 64), strings.Repeat("0", 66), ""} {
		if _, err := ParseKey(in); err == nil {
			t.Errorf("%q parsed", in)
		}
	}
	if _, err := ParseKey("hlk_abc"); !errors.Is(err, ErrIntegrationKey) {
		t.Errorf("hlk_ key: %v", err)
	}
}

func TestGenerate(t *testing.T) {
	seed := bytes.Repeat([]byte{7}, keySize)
	k, err := Generate(bytes.NewReader(seed))
	if err != nil {
		t.Fatal(err)
	}
	if k.Hex() != hex.EncodeToString(seed) || len(k.ID()) != 8 {
		t.Errorf("hex %s id %s", k.Hex(), k.ID())
	}
	if _, err := Generate(bytes.NewReader(seed[:5])); err == nil {
		t.Error("short random source accepted")
	}
}

// A tampered envelope that still parses must fail authentication, never
// decode into something.
func TestOpenRejectsAnyBitFlip(t *testing.T) {
	k := mustKey(t, strings.Repeat("11", keySize))
	env, err := k.Seal(Message{Title: "t", Body: "b"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, sealed, _ := ParseEnvelope(env)
	for i := range sealed {
		b := bytes.Clone(sealed)
		b[i] ^= 0x80
		if _, err := k.Open("pw1." + k.ID() + "." + base64.RawURLEncoding.EncodeToString(b)); err == nil {
			t.Fatalf("byte %d flipped and still opened", i)
		}
	}
}
