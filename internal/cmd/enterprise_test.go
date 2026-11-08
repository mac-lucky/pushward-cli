package cmd

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testDomainID = "5b0e2f4a-9c1d-4e7b-8a3f-2d6c1b0e9f7a"

// testIDNID is the id of the internationalized domain in testDomains, which
// the server lists in punycode.
const testIDNID = "6c1f3a5b-0d2e-4f8c-9b4a-3e7d2c1f0a8b"

const testDomains = `{"domains":[{"id":"` + testDomainID + `","domain":"example.com","status":"pending","txt_name":"_pushward-verification.example.com","txt_value":"pushward-verification=abc","created_at":"2026-11-01T10:00:00Z"},` +
	`{"id":"` + testIDNID + `","domain":"xn--bcher-kva.example","status":"pending","txt_name":"_pushward-verification.xn--bcher-kva.example","txt_value":"pushward-verification=def","created_at":"2026-11-01T10:00:00Z"}]}`

const notVerified = `{"status":422,"code":"org.domain_not_verified","detail":"the TXT record was not found"}`

func TestOrgDomainCommands(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_ORG_API_TOKEN", "hlo_test")
	f.reply("GET /orgs", 200, testOrgs)
	f.reply("GET /orgs/o1/domains", 200, testDomains)
	f.reply("POST /orgs/o1/domains", 201, `{"id":"`+testDomainID+`","domain":"example.com","status":"pending","txt_name":"_pushward-verification.example.com","txt_value":"pushward-verification=abc"}`)
	f.reply("POST /orgs/o1/domains/"+testDomainID+"/verify", 200, `{"id":"`+testDomainID+`","domain":"example.com","status":"verified"}`)
	f.reply("DELETE /orgs/o1/domains/"+testDomainID, 204, "")
	f.reply("DELETE /orgs/o1/domains/"+testIDNID, 204, "")
	for _, tc := range []struct {
		argv   []string
		routes []string // what the command sends after GET /orgs
		body   string   // the last request's body
	}{
		{[]string{"org", "domain", "list"}, []string{"GET /orgs/o1/domains"}, ""},
		{[]string{"org", "domain", "add", "example.com"}, []string{"POST /orgs/o1/domains"}, `{"domain":"example.com"}`},
		{[]string{"org", "domain", "verify", testDomainID}, []string{"POST /orgs/o1/domains/" + testDomainID + "/verify"}, ""},
		{[]string{"org", "domain", "verify", "Example.COM."}, []string{"GET /orgs/o1/domains", "POST /orgs/o1/domains/" + testDomainID + "/verify"}, ""},
		{[]string{"org", "domain", "remove", "example.com"}, []string{"GET /orgs/o1/domains", "DELETE /orgs/o1/domains/" + testDomainID}, ""},
		{[]string{"org", "domain", "remove", "B\u00fccher.example"}, []string{"GET /orgs/o1/domains", "DELETE /orgs/o1/domains/" + testIDNID}, ""},
		{[]string{"org", "domain", "remove", "xn--bcher-kva.example"}, []string{"GET /orgs/o1/domains", "DELETE /orgs/o1/domains/" + testIDNID}, ""},
	} {
		f.calls = nil
		r := runCLI(t, srv, nil, "", tc.argv...)
		if r.code != 0 {
			t.Errorf("%v: exit %d: %s", tc.argv, r.code, r.stderr)
			continue
		}
		var got []string
		for _, c := range f.calls[1:] {
			got = append(got, c.Method+" "+c.Path)
		}
		if strings.Join(got, ", ") != strings.Join(tc.routes, ", ") {
			t.Errorf("%v: sent %v, want %v", tc.argv, got, tc.routes)
			continue
		}
		last := f.calls[len(f.calls)-1]
		body := ""
		if last.Body != nil {
			body = mustJSON(t, last.Body)
		}
		if body != tc.body {
			t.Errorf("%v: body %s, want %s", tc.argv, body, tc.body)
		}
	}

	r := runWith(t, srv, func(a *App) { a.StdoutTTY = true }, "org", "domain", "add", "example.com")
	for _, want := range []string{"name   _pushward-verification.example.com", "value  pushward-verification=abc", "pushward org domain verify example.com"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("domain add on a terminal printed %q, want %q in it", r.stdout, want)
		}
	}
	r = runWith(t, srv, func(a *App) { a.StdoutTTY = true }, "org", "domain", "list")
	if !strings.Contains(r.stdout, "example.com") || !strings.Contains(r.stdout, "pending") {
		t.Errorf("domain list on a terminal printed %q", r.stdout)
	}

	f.calls = nil
	r = runCLI(t, srv, nil, "", "org", "domain", "remove", "other.example")
	if r.code != ExitNotFound || !strings.Contains(r.stderr, "no domain other.example") {
		t.Errorf("unknown domain: exit %d, stderr %q; want %d", r.code, r.stderr, ExitNotFound)
	}
	for _, c := range f.calls {
		if c.Method == http.MethodDelete {
			t.Errorf("unknown domain sent %s %s", c.Method, c.Path)
		}
	}
}

func TestOrgDomainVerifyWait(t *testing.T) {
	route := "POST /orgs/o1/domains/" + testDomainID + "/verify"
	const noAnswer = `{"status":503,"code":"service.unavailable","detail":"the lookup did not answer"}`
	verified := `{"domain":"example.com","status":"verified"}`
	for _, tc := range []struct {
		name     string
		wait     string
		answers  []string // refusals until a 200, in order; the last one repeats
		code     int
		attempts int
		sleeps   int // the 15s ones between attempts
	}{
		{"plain verify tries once", "", []string{notVerified}, ExitError, 1, 0},
		{"record shows up", "1m", []string{notVerified, notVerified, verified}, ExitOK, 3, 2},
		{"wait runs out", "20s", []string{notVerified}, ExitWaitTimeout, 2, 1},
		{"other refusals end it", "1m", []string{`{"status":409,"code":"org.domain_taken","detail":"another organization verified it"}`}, ExitError, 1, 0},
		// The API client retries a 503 five times by itself before the loop sees it.
		{"lookup without an answer", "1m", []string{noAnswer, noAnswer, noAnswer, noAnswer, noAnswer, verified}, ExitOK, 6, 1},
		{"plain verify without an answer", "", []string{noAnswer}, ExitUnavailable, 5, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newFake(t)
			t.Setenv("PUSHWARD_ORG_API_TOKEN", "hlo_test")
			f.reply("GET /orgs", 200, testOrgs)
			n := 0
			f.routes[route] = func(call) (int, string) {
				body := tc.answers[min(n, len(tc.answers)-1)]
				n++
				if strings.Contains(body, `"status":422`) {
					return 422, body
				}
				if strings.Contains(body, `"status":409`) {
					return 409, body
				}
				if strings.Contains(body, `"status":503`) {
					return 503, body
				}
				return 200, body
			}
			argv := []string{"org", "domain", "verify", testDomainID}
			if tc.wait != "" {
				argv = append(argv, "--wait", tc.wait)
			}
			r := runCLI(t, srv, nil, "", argv...)
			waits := 0
			for _, d := range r.sleeps {
				if d == 15*time.Second {
					waits++
				}
			}
			if r.code != tc.code || n != tc.attempts || waits != tc.sleeps {
				t.Fatalf("exit %d after %d attempts and sleeps %v, want exit %d, %d attempts, %d sleeps of 15s; stderr %q",
					r.code, n, r.sleeps, tc.code, tc.attempts, tc.sleeps, r.stderr)
			}
			if tc.code == ExitWaitTimeout && !strings.Contains(r.stderr, "org.domain_not_verified") {
				t.Errorf("timeout stderr %q, want the server's refusal", r.stderr)
			}
			if tc.wait != "" && tc.answers[0] == noAnswer && !strings.Contains(r.stderr, "the lookup of "+testDomainID+" got no answer; trying again in 15s") {
				t.Errorf("stderr %q, want the retry explained", r.stderr)
			}
		})
	}
}

// verify is safe to repeat, so --wait tries again after a request that got
// no response, which the API client never does for a POST by itself.
func TestOrgDomainVerifyConnectionFails(t *testing.T) {
	for _, tc := range []struct {
		wait     string
		code     int
		attempts int // the ones that reached the server
	}{
		{"", ExitUnavailable, 0},
		{"1m", ExitOK, 1},
	} {
		f, srv := newFake(t)
		t.Setenv("PUSHWARD_ORG_API_TOKEN", "hlo_test")
		f.reply("GET /orgs", 200, testOrgs)
		f.reply("POST /orgs/o1/domains/"+testDomainID+"/verify", 200, `{"domain":"example.com","status":"verified"}`)
		argv := []string{"org", "domain", "verify", testDomainID}
		if tc.wait != "" {
			argv = append(argv, "--wait", tc.wait)
		}
		dropped := false
		r := runWith(t, srv, func(a *App) {
			next := srv.Client().Transport
			a.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodPost && !dropped {
					dropped = true
					return nil, errors.New("connection reset by peer")
				}
				return next.RoundTrip(req)
			})}
		}, argv...)
		attempts := 0
		for _, c := range f.calls {
			if c.Method == http.MethodPost {
				attempts++
			}
		}
		if r.code != tc.code || attempts != tc.attempts {
			t.Errorf("--wait %q: exit %d after %d attempts, want exit %d after %d; stderr %q", tc.wait, r.code, attempts, tc.code, tc.attempts, r.stderr)
		}
		if tc.wait != "" && !strings.Contains(r.stderr, "connection reset by peer; trying again in 15s") {
			t.Errorf("stderr %q, want the retry explained", r.stderr)
		}
	}
}

func TestOrgSSOSet(t *testing.T) {
	const none = `{"configured":false,"redirect_uri":"https://console.example/auth/sso"}`
	const storedOIDC = `{"configured":true,"protocol":"oidc","enabled":false,"require_sso":true,"oidc":{"issuer":"https://idp.example","client_id":"pw","client_secret_set":true}}`
	const storedSAML = `{"configured":true,"protocol":"saml","enabled":true,"require_sso":false,"saml":{"idp_entity_id":"urn:idp"}}`
	metadata := filepath.Join(t.TempDir(), "idp.xml")
	if err := os.WriteFile(metadata, []byte("<EntityDescriptor/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		stored string
		stdin  string
		argv   []string
		body   string
	}{
		{"oidc set up", none, "s3cret-value\n",
			[]string{"--protocol", "oidc", "--issuer", "https://idp.example", "--client-id", "pw", "--client-secret-stdin"},
			`{"enabled":false,"oidc":{"client_id":"pw","client_secret":"s3cret-value","issuer":"https://idp.example"},"protocol":"oidc","require_sso":false}`},
		{"turn on keeps the rest", storedOIDC, "", []string{"--enabled"},
			`{"enabled":true,"oidc":{"client_id":"pw","issuer":"https://idp.example"},"protocol":"oidc","require_sso":true}`},
		{"turn off", storedOIDC, "", []string{"--require-sso=false"},
			`{"enabled":false,"oidc":{"client_id":"pw","issuer":"https://idp.example"},"protocol":"oidc","require_sso":false}`},
		{"new client id", storedOIDC, "other\n", []string{"--client-id", "pw2", "--client-secret-stdin"},
			`{"enabled":false,"oidc":{"client_id":"pw2","client_secret":"other","issuer":"https://idp.example"},"protocol":"oidc","require_sso":true}`},
		{"saml from a file", none, "", []string{"--protocol", "saml", "--metadata-file", metadata, "--enabled"},
			`{"enabled":true,"protocol":"saml","require_sso":false,"saml":{"idp_metadata_xml":"\u003cEntityDescriptor/\u003e"}}`},
		{"saml from a url", storedOIDC, "", []string{"--protocol", "saml", "--metadata-url", "https://idp.example/metadata"},
			`{"enabled":false,"protocol":"saml","require_sso":true,"saml":{"idp_metadata_url":"https://idp.example/metadata"}}`},
		{"saml keeps its provider", storedSAML, "", []string{"--require-sso"},
			`{"enabled":true,"protocol":"saml","require_sso":true,"saml":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, srv := newFake(t)
			t.Setenv("PUSHWARD_ORG_API_TOKEN", "hlo_test")
			f.reply("GET /orgs", 200, testOrgs)
			f.reply("GET /orgs/o1/sso", 200, tc.stored)
			f.reply("PUT /orgs/o1/sso", 200, `{"configured":true,"protocol":"oidc","enabled":true,"require_sso":false}`)
			r := runCLI(t, srv, nil, tc.stdin, append([]string{"org", "sso", "set"}, tc.argv...)...)
			if r.code != 0 {
				t.Fatalf("exit %d: %s", r.code, r.stderr)
			}
			last := f.calls[len(f.calls)-1]
			if last.Method+" "+last.Path != "PUT /orgs/o1/sso" || f.calls[len(f.calls)-2].Path != "/orgs/o1/sso" {
				t.Fatalf("calls %+v, want GET then PUT /orgs/o1/sso", f.calls)
			}
			if got := mustJSON(t, last.Body); got != tc.body {
				t.Errorf("body %s, want %s", got, tc.body)
			}
			if strings.Contains(r.stdout+r.stderr, "s3cret") {
				t.Errorf("the secret was printed: %q %q", r.stdout, r.stderr)
			}
		})
	}
}

func TestOrgSSOSetRefusals(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_ORG_API_TOKEN", "hlo_test")
	f.reply("GET /orgs", 200, testOrgs)
	f.reply("GET /orgs/o1/sso", 200, `{"configured":true,"protocol":"saml","enabled":true,"require_sso":false}`)
	for _, tc := range []struct {
		argv  []string
		stdin string
	}{
		{[]string{"--protocol", "ldap"}, ""},
		{[]string{"--issuer", "https://idp.example", "--metadata-url", "https://idp.example/m"}, ""},
		{[]string{"--protocol", "saml", "--client-id", "pw"}, ""},
		{[]string{"--protocol", "oidc", "--metadata-file", "x.xml"}, ""},
		{[]string{"--protocol", "saml", "--metadata-file", "x.xml", "--metadata-url", "https://idp.example/m"}, ""},
		{[]string{"--protocol", "saml", "--metadata-file", filepath.Join(t.TempDir(), "missing.xml")}, ""},
		{[]string{"--protocol", "oidc", "--issuer", "https://idp.example", "--client-id", "pw", "--client-secret-stdin"}, "\n"},
		{[]string{"--protocol", "oidc", "--issuer", "https://idp.example"}, ""}, // no client id stored for oidc
		{[]string{"--issuer", "https://idp.example"}, ""},                       // the stored protocol is saml
		{[]string{"--enabled", "false"}, ""},
	} {
		f.calls = nil
		r := runCLI(t, srv, nil, tc.stdin, append([]string{"org", "sso", "set"}, tc.argv...)...)
		if r.code != ExitUsage {
			t.Errorf("%v: exit %d (%s), want %d", tc.argv, r.code, r.stderr, ExitUsage)
		}
		for _, c := range f.calls {
			if c.Method == http.MethodPut {
				t.Errorf("%v: sent PUT", tc.argv)
			}
		}
	}

	f.reply("GET /orgs/o1/sso", 200, `{"configured":false}`)
	if r := runCLI(t, srv, nil, "", "org", "sso", "set", "--enabled"); r.code != ExitUsage || !strings.Contains(r.stderr, "--protocol") {
		t.Errorf("nothing stored and no --protocol: exit %d, %q", r.code, r.stderr)
	}

	const denied = `{"status":403,"code":"org.permission_denied","detail":"your role in this organization does not allow this"}`
	f.reply("PUT /orgs/o1/sso", 403, denied)
	r := runCLI(t, srv, nil, "", "org", "sso", "set", "--protocol", "saml", "--metadata-url", "https://idp.example/m")
	if r.code != ExitAuth || !strings.Contains(r.stderr, "your role in this organization does not allow this") ||
		!strings.Contains(r.stderr, "\nhint: an owner can make this change in the PushWard console") {
		t.Errorf("owner-only refusal: exit %d, stderr %q", r.code, r.stderr)
	}

	// The hint is for a change of identity provider only: the same 403 on a
	// toggle or a new secret is a key below manage, which an owner does not fix.
	const storedOIDC = `{"configured":true,"protocol":"oidc","enabled":true,"require_sso":false,"oidc":{"issuer":"https://idp.example","client_id":"pw"}}`
	for _, tc := range []struct {
		stored string
		stdin  string
		argv   []string
		hint   bool
	}{
		{storedOIDC, "", []string{"--require-sso"}, false},
		{storedOIDC, "new\n", []string{"--client-id", "pw", "--client-secret-stdin"}, false},
		{storedOIDC, "new\n", []string{"--issuer", "https://other.example", "--client-secret-stdin"}, true},
		{storedOIDC, "", []string{"--protocol", "saml", "--metadata-url", "https://idp.example/m"}, true},
		{`{"configured":true,"protocol":"saml","enabled":true,"require_sso":false}`, "", []string{"--enabled=false"}, false},
	} {
		f.reply("GET /orgs/o1/sso", 200, tc.stored)
		r := runCLI(t, srv, nil, tc.stdin, append([]string{"org", "sso", "set"}, tc.argv...)...)
		if r.code != ExitAuth || strings.Contains(r.stderr, "hint:") != tc.hint {
			t.Errorf("%v refused: exit %d, stderr %q, want the hint %v", tc.argv, r.code, r.stderr, tc.hint)
		}
	}
}

func TestOrgSSOShow(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_ORG_API_TOKEN", "hlo_test")
	f.reply("GET /orgs", 200, testOrgs)
	const conn = `{"configured":true,"protocol":"saml","enabled":true,"require_sso":false,"redirect_uri":"https://console.example/auth/sso","saml":{"sp_entity_id":"https://api.example/auth/saml/o1","acs_url":"https://api.example/auth/saml/o1/acs"}}`
	f.reply("GET /orgs/o1/sso", 200, conn)
	r := runCLI(t, srv, nil, "", "org", "sso", "show")
	if r.code != 0 || len(f.calls) != 2 || f.calls[1].Method+" "+f.calls[1].Path != "GET /orgs/o1/sso" {
		t.Fatalf("exit %d (%s), calls %+v", r.code, r.stderr, f.calls)
	}
	if f.calls[1].Auth != "Bearer hlo_test" {
		t.Errorf("sent %q, want the org admin key", f.calls[1].Auth)
	}
	if !strings.Contains(r.stdout, `"acs_url":"https://api.example/auth/saml/o1/acs"`) || !strings.Contains(r.stdout, "https://console.example/auth/sso") {
		t.Errorf("stdout %q, want the connection", r.stdout)
	}
}

func TestOrgSSODelete(t *testing.T) {
	f, srv := newFake(t)
	t.Setenv("PUSHWARD_ORG_API_TOKEN", "hlo_test")
	f.reply("GET /orgs", 200, testOrgs)
	f.reply("DELETE /orgs/o1/sso", 204, "")
	for _, tc := range []struct {
		argv    []string
		stdin   string
		deleted bool
	}{
		{nil, "", false},
		{nil, "n\n", false},
		{nil, "y\n", true},
		{nil, "YES", true},
		{[]string{"--yes"}, "", true},
	} {
		f.calls = nil
		r := runCLI(t, srv, nil, tc.stdin, append([]string{"org", "sso", "delete"}, tc.argv...)...)
		deleted := false
		for _, c := range f.calls {
			deleted = deleted || c.Method+" "+c.Path == "DELETE /orgs/o1/sso"
		}
		asked := strings.Contains(r.stderr, "[y/N]")
		if deleted != tc.deleted || (r.code == 0) != tc.deleted || asked == (tc.argv != nil) {
			t.Errorf("%v with %q: exit %d, deleted %v, asked %v; stderr %q", tc.argv, tc.stdin, r.code, deleted, asked, r.stderr)
		}
	}
}

// exported is what the last export request asked for, and how many came.
type exported struct {
	mu    sync.Mutex
	query url.Values
	hdr   http.Header
	n     int
}

func (e *exported) get() (url.Values, http.Header) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.query, e.hdr
}

func (e *exported) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.n
}

// exportServer answers GET /orgs and the audit export with csv, recording
// the export request.
func exportServer(t *testing.T, csv func(w http.ResponseWriter)) (*httptest.Server, *exported) {
	t.Helper()
	newFake(t)
	t.Setenv("PUSHWARD_ORG_API_TOKEN", "hlo_test")
	seen := &exported{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs":
			io.WriteString(w, testOrgs)
		case "/orgs/o1/audit-events/export":
			seen.mu.Lock()
			seen.query, seen.hdr = r.URL.Query(), r.Header.Clone()
			seen.n++
			seen.mu.Unlock()
			csv(w)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("PUSHWARD_API_URL", srv.URL)
	return srv, seen
}

const testCSV = "id,time,actor_kind\n1,2026-01-01T00:00:00Z,user\n2,2026-01-02T00:00:00Z,key\n"

func writeCSV(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="pushward-audit-acme-20260101-20260201.csv"`)
	for line := range strings.Lines(testCSV) {
		io.WriteString(w, line)
		w.(http.Flusher).Flush()
	}
}

func TestOrgAuditExport(t *testing.T) {
	srv, seen := exportServer(t, writeCSV)

	dir := t.TempDir()
	t.Chdir(dir)
	r := runCLI(t, srv, nil, "", "org", "audit", "export", "--from", "2026-01-01", "--to", "2026-02-01T10:00:00+02:00")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	query, hdr := seen.get()
	if query.Get("from") != "2026-01-01T00:00:00Z" || query.Get("to") != "2026-02-01T08:00:00Z" {
		t.Errorf("query %v, want the range in UTC", query)
	}
	if !strings.Contains(hdr.Get("Accept"), "text/csv") || hdr.Get("Authorization") != "Bearer hlo_test" {
		t.Errorf("headers %v", hdr)
	}
	data, err := os.ReadFile(filepath.Join(dir, "pushward-audit-acme-20260101-20260201.csv"))
	if err != nil || string(data) != testCSV {
		t.Fatalf("saved %q (%v), want the CSV under the server's file name", data, err)
	}
	want := `{"bytes":` + strconv.Itoa(len(testCSV)) + `,"path":"pushward-audit-acme-20260101-20260201.csv"}`
	if strings.TrimSpace(r.stdout) != want || string(r.app.Body) != want {
		t.Errorf("piped output %q, Body %s, want %s", r.stdout, r.app.Body, want)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("left %d files behind, want 1", len(entries))
	}

	// The server's file name never replaces a file that is there.
	if err := os.WriteFile(filepath.Join(dir, "pushward-audit-acme-20260101-20260201.csv"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = runCLI(t, srv, nil, "", "org", "audit", "export")
	data, _ = os.ReadFile(filepath.Join(dir, "pushward-audit-acme-20260101-20260201.csv"))
	if r.code == 0 || !strings.Contains(r.stderr, "already exists; name the file with -o") || string(data) != "mine" {
		t.Errorf("existing file: exit %d, stderr %q, file %q", r.code, r.stderr, data)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("a refused save left %d files, want 1", len(entries))
	}

	// A place it cannot save to fails before the export is asked for.
	before := seen.count()
	r = runCLI(t, srv, nil, "", "org", "audit", "export", "-o", filepath.Join(dir, "missing", "audit.csv"))
	if r.code == 0 || seen.count() != before {
		t.Errorf("missing directory: exit %d, %d exports asked for, want none", r.code, seen.count()-before)
	}
	r = runCLI(t, srv, nil, "", "org", "audit", "export", "-o", dir)
	if r.code != ExitUsage || seen.count() != before || !strings.Contains(r.stderr, "is a directory") {
		t.Errorf("-o a directory: exit %d, %d exports asked for, stderr %q", r.code, seen.count()-before, r.stderr)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("-o a directory left %d files, want 1", len(entries))
	}

	r = runCLI(t, srv, nil, "", "org", "audit", "export", "-o", "-")
	if r.code != 0 || r.stdout != testCSV {
		t.Errorf("-o -: exit %d, stdout %q", r.code, r.stdout)
	}
	if query, _ := seen.get(); len(query) != 0 {
		t.Errorf("no range sent %v, want the server's defaults", query)
	}

	named := filepath.Join(t.TempDir(), "audit.csv")
	if err := os.WriteFile(named, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = runCLI(t, srv, nil, "", "org", "audit", "export", "--output", named)
	if data, _ := os.ReadFile(named); r.code != 0 || string(data) != testCSV {
		t.Errorf("-o file: exit %d, saved %q", r.code, data)
	}

	if r := runCLI(t, srv, nil, "", "org", "audit", "export", "--from", "last week"); r.code != ExitUsage {
		t.Errorf("bad --from: exit %d, want %d", r.code, ExitUsage)
	}
}

// The export is never held in memory whole: one past the API client's
// response cap still saves.
func TestOrgAuditExportStreams(t *testing.T) {
	const size = 9 << 20
	srv, _ := exportServer(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/csv")
		chunk := strings.Repeat("x", 1<<16-1) + "\n"
		for range size / len(chunk) {
			io.WriteString(w, chunk)
		}
	})
	out := filepath.Join(t.TempDir(), "big.csv")
	r := runCLI(t, srv, nil, "", "org", "audit", "export", "-o", out)
	info, err := os.Stat(out)
	if r.code != 0 || err != nil || info.Size() < size-1<<16 {
		t.Fatalf("exit %d (%s), file %v %v", r.code, r.stderr, info, err)
	}
}

func TestOrgAuditExportFailures(t *testing.T) {
	// A connection that drops mid-file fails and leaves nothing behind.
	srv, _ := exportServer(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Length", "1000")
		io.WriteString(w, "id,time\n1,")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})
	dir := t.TempDir()
	r := runCLI(t, srv, nil, "", "org", "audit", "export", "-o", filepath.Join(dir, "audit.csv"))
	if entries, _ := os.ReadDir(dir); r.code == 0 || len(entries) != 0 {
		t.Errorf("dropped stream: exit %d, %d files left", r.code, len(entries))
	}

	srv, _ = exportServer(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(422)
		io.WriteString(w, `{"status":422,"code":"org.audit_range_invalid","detail":"from must not be after to"}`)
	})
	r = runCLI(t, srv, nil, "", "org", "audit", "export", "-o", filepath.Join(dir, "audit.csv"))
	if r.code != ExitError || !strings.Contains(r.stderr, "org.audit_range_invalid: from must not be after to") {
		t.Errorf("refusal: exit %d, stderr %q", r.code, r.stderr)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a refused export left %d files", len(entries))
	}

	// Every try that reaches the server counts as an export: a 500 is not
	// tried again, as it would be for another GET.
	srv, seen := exportServer(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(500)
		io.WriteString(w, `{"status":500,"code":"internal","detail":"something broke"}`)
	})
	r = runCLI(t, srv, nil, "", "org", "audit", "export", "-o", filepath.Join(dir, "audit.csv"))
	if r.code != ExitUnavailable || seen.count() != 1 || len(r.sleeps) != 0 {
		t.Errorf("500: exit %d after %d exports and sleeps %v, want exit %d after one", r.code, seen.count(), r.sleeps, ExitUnavailable)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a failed export left %d files", len(entries))
	}
}

// A file that shows up under the server's name while the export downloads
// is refused as well, not replaced.
func TestOrgAuditExportNameTakenMeanwhile(t *testing.T) {
	const name = "pushward-audit-acme-20260101-20260201.csv"
	dir := t.TempDir()
	t.Chdir(dir)
	srv, _ := exportServer(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		io.WriteString(w, "id,time\n")
		w.(http.Flusher).Flush()
		time.Sleep(50 * time.Millisecond) // past any look at the name when the headers came
		if err := os.WriteFile(filepath.Join(dir, name), []byte("mine"), 0o600); err != nil {
			t.Error(err)
		}
		io.WriteString(w, "1,2026-01-01T00:00:00Z\n")
	})
	r := runCLI(t, srv, nil, "", "org", "audit", "export")
	data, _ := os.ReadFile(filepath.Join(dir, name))
	if r.code == 0 || !strings.Contains(r.stderr, name+" already exists; name the file with -o") || string(data) != "mine" {
		t.Errorf("exit %d, stderr %q, file %q", r.code, r.stderr, data)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("left %d files, want 1", len(entries))
	}
}

func TestAttachmentName(t *testing.T) {
	for header, want := range map[string]string{
		`attachment; filename="pushward-audit-acme-20260101-20260201.csv"`: "pushward-audit-acme-20260101-20260201.csv",
		`attachment; filename="../../etc/passwd"`:                          "",
		`attachment; filename="dir/a.csv"`:                                 "",
		`attachment; filename="..\a.csv"`:                                  "",
		`attachment; filename=".."`:                                        "",
		`attachment; filename=".bashrc"`:                                   "",
		`attachment; filename=.git`:                                        "",
		`attachment`:                                                       "",
		``:                                                                 "",
	} {
		if got := attachmentName(header); got != want {
			t.Errorf("attachmentName(%q) = %q, want %q", header, got, want)
		}
	}
}
