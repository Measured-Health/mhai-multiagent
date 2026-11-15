package s3

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Synthetic ids and a test-only credential (no real store accepts it).
const (
	hosp   = "22222222-2222-4222-8222-222222222222"
	doc    = "55555555-5555-4555-8555-555555555555"
	akid   = "TESTONLYDOCS"
	secret = "test-only-documents-store-secret"
)

var now = time.Date(2026, 11, 15, 9, 30, 0, 0, time.UTC)

func newClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	c, err := New(Config{Endpoint: endpoint, Bucket: "mhai-documents-test", Region: "us-east-1",
		Credentials: Credentials{AccessKeyID: akid, Secret: secret}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConfigIsChecked(t *testing.T) {
	good := Config{Endpoint: "https://store.example.test", Bucket: "mhai-documents-dev", Region: "eu-west-1",
		Credentials: Credentials{AccessKeyID: akid, Secret: secret}}
	if err := CheckConfig(good); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Config){
		"ftp endpoint":    func(c *Config) { c.Endpoint = "ftp://store.example.test" },
		"credentials":     func(c *Config) { c.Endpoint = "https://u:p@store.example.test" },
		"path":            func(c *Config) { c.Endpoint = "https://store.example.test/bucket" },
		"query":           func(c *Config) { c.Endpoint = "https://store.example.test?x=1" },
		"no host":         func(c *Config) { c.Endpoint = "https://" },
		"bucket upper":    func(c *Config) { c.Bucket = "Documents" },
		"bucket short":    func(c *Config) { c.Bucket = "ab" },
		"region":          func(c *Config) { c.Region = "EU WEST" },
		"access key":      func(c *Config) { c.Credentials.AccessKeyID = "a/b" },
		"short secret":    func(c *Config) { c.Credentials.Secret = "short" },
		"bucket with dir": func(c *Config) { c.Bucket = "a/b/c" },
	} {
		c := good
		mut(&c)
		err := CheckConfig(c)
		if !errors.Is(err, ErrConfig) {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "u:p") {
			t.Errorf("%s: the error carries a value", name)
		}
		if _, err := New(c); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: New %v", name, err)
		}
	}
}

func TestAllowedKeys(t *testing.T) {
	for _, k := range []string{"quarantine/" + hosp + "/" + doc, "originals/" + hosp + "/" + doc,
		"pages/" + hosp + "/" + doc + "/1", "pages/" + hosp + "/" + doc + "/50"} {
		if !AllowedKey(k) {
			t.Errorf("%s refused", k)
		}
	}
	for _, k := range []string{"", "quarantine/", "quarantine/" + hosp, "pages/" + hosp + "/" + doc + "/0",
		"pages/" + hosp + "/" + doc + "/51", "pages/" + hosp + "/" + doc, "originals/" + hosp + "/" + doc + "/1",
		"originals/" + hosp + "/../" + doc, "originals/AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE/" + doc,
		"models/" + hosp + "/" + doc, "originals/" + hosp + "/" + doc + "?versionId=1", "/originals/" + hosp + "/" + doc,
		"originals/" + hosp + "/lab report.pdf"} {
		if AllowedKey(k) {
			t.Errorf("%q allowed", k)
		}
	}
}

// sigCheck re-derives a request's SigV4 signature the way S3 does, from
// what arrived: the canonical request is built from the received method,
// path, query and the signed headers.
func sigCheck(t *testing.T, r *http.Request, body []byte) {
	t.Helper()
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, Algorithm+" Credential="+akid+"/20261115/us-east-1/s3/aws4_request, SignedHeaders=") {
		t.Fatalf("authorization %q", auth)
	}
	parts := strings.Split(auth, ", ")
	signed := strings.TrimPrefix(parts[1], "SignedHeaders=")
	sig := strings.TrimPrefix(parts[2], "Signature=")
	payload := sha256.Sum256(body)
	if r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(payload[:]) {
		t.Fatal("payload hash")
	}
	var ch strings.Builder
	for _, h := range strings.Split(signed, ";") {
		v := r.Header.Get(h)
		if h == "host" {
			v = r.Host
		}
		ch.WriteString(h + ":" + v + "\n")
	}
	q := ""
	if r.URL.RawQuery != "" {
		q = r.URL.RawQuery + "="
	}
	canon := r.Method + "\n" + r.URL.Path + "\n" + q + "\n" + ch.String() + "\n" + signed + "\n" + hex.EncodeToString(payload[:])
	cs := sha256.Sum256([]byte(canon))
	toSign := Algorithm + "\n20261115T093000Z\n20261115/us-east-1/s3/aws4_request\n" + hex.EncodeToString(cs[:])
	k := []byte("AWS4" + secret)
	for _, p := range []string{"20261115", "us-east-1", "s3", "aws4_request"} {
		m := hmac.New(sha256.New, k)
		m.Write([]byte(p))
		k = m.Sum(nil)
	}
	m := hmac.New(sha256.New, k)
	m.Write([]byte(toSign))
	if hex.EncodeToString(m.Sum(nil)) != sig {
		t.Fatal("signature does not verify")
	}
	for _, h := range []string{"host", "x-amz-content-sha256", "x-amz-date"} {
		if !strings.Contains(";"+signed+";", ";"+h+";") {
			t.Fatalf("%s not signed", h)
		}
	}
}

func TestClientSignsAndMapsStatuses(t *testing.T) {
	key := "originals/" + hosp + "/" + doc
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sigCheck(t, r, body)
		got = append(got, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.Method == http.MethodPut && r.URL.RawQuery == "" && strings.Contains(r.URL.Path, "originals/"):
			if string(body) != "sealed" || r.Header.Get("Content-Type") != "application/octet-stream" {
				t.Error("put body or type")
			}
		case r.Method == http.MethodGet && r.URL.RawQuery == "lifecycle":
			_, _ = w.Write([]byte("<LifecycleConfiguration/>"))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte("sealed"))
		case r.Method == http.MethodHead:
			w.Header().Set("Content-Length", "6")
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPut && r.URL.RawQuery != "":
			if r.Header.Get("Content-Md5") == "" {
				t.Error("bucket configuration without Content-MD5")
			}
		}
	}))
	defer srv.Close()
	c := newClient(t, srv.URL)
	ctx := context.Background()
	if err := c.Put(ctx, key, []byte("sealed"), "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	rc, err := c.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "sealed" {
		t.Fatal(string(b))
	}
	if n, err := c.Head(ctx, key); err != nil || n != 6 {
		t.Fatalf("head %d %v", n, err)
	}
	if err := c.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if lc, err := c.Lifecycle(ctx); err != nil || string(lc) != "<LifecycleConfiguration/>" {
		t.Fatalf("lifecycle %q %v", lc, err)
	}
	if err := c.CreateBucket(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.PutVersioning(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.PutLifecycle(ctx, []byte("<LifecycleConfiguration/>")); err != nil {
		t.Fatal(err)
	}
	want := []string{"PUT /mhai-documents-test/" + key, "GET /mhai-documents-test/" + key, "HEAD /mhai-documents-test/" + key,
		"DELETE /mhai-documents-test/" + key, "GET /mhai-documents-test?lifecycle", "PUT /mhai-documents-test",
		"PUT /mhai-documents-test?versioning", "PUT /mhai-documents-test?lifecycle"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("requests:\n%s", strings.Join(got, "\n"))
	}
	if c.Bucket() != "mhai-documents-test" || c.Region() != "us-east-1" {
		t.Fatal("accessors")
	}
}

func TestClientErrors(t *testing.T) {
	status := http.StatusNotFound
	redirected := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			redirected = true
			return
		}
		if status == http.StatusFound {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte("<Error><Message>synthetic detail " + secret + "</Message></Error>"))
	}))
	defer srv.Close()
	c := newClient(t, srv.URL)
	ctx := context.Background()
	key := "pages/" + hosp + "/" + doc + "/2"
	if _, err := c.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get 404: %v", err)
	}
	if _, err := c.Head(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("head 404: %v", err)
	}
	if err := c.Delete(ctx, key); err != nil {
		t.Fatalf("delete 404 is done: %v", err)
	}
	if _, err := c.Lifecycle(ctx); !errors.Is(err, ErrNoLifecycle) {
		t.Fatalf("lifecycle 404: %v", err)
	}
	for _, status = range []int{http.StatusForbidden, http.StatusInternalServerError, http.StatusFound} {
		var se *StatusError
		errs := []error{c.Put(ctx, key, []byte("x"), "application/octet-stream"), c.Delete(ctx, key), c.PutVersioning(ctx)}
		_, e1 := c.Get(ctx, key)
		_, e2 := c.Head(ctx, key)
		_, e3 := c.Lifecycle(ctx)
		errs = append(errs, e1, e2, e3)
		for i, err := range errs {
			if !errors.As(err, &se) || se.Code != status {
				t.Errorf("status %d call %d: %v", status, i, err)
			}
			if err != nil && strings.Contains(err.Error(), secret) {
				t.Errorf("status %d call %d: the error carries the body", status, i)
			}
		}
	}
	if redirected {
		t.Fatal("a redirect was followed")
	}
	// Keys outside the three prefixes never leave the process.
	for _, err := range []error{c.Put(ctx, "models/x", nil, ""), c.Delete(ctx, "quarantine/x")} {
		if !errors.Is(err, ErrKey) {
			t.Fatalf("key: %v", err)
		}
	}
	if _, err := c.Get(ctx, "../etc"); !errors.Is(err, ErrKey) {
		t.Fatal(err)
	}
	if _, err := c.Head(ctx, ""); !errors.Is(err, ErrKey) {
		t.Fatal(err)
	}
	// An unreachable store: an error that names neither the URL nor a key.
	srv.Close()
	_, err := c.Get(ctx, key)
	if err == nil || strings.Contains(err.Error(), srv.URL) || strings.Contains(err.Error(), doc) {
		t.Fatalf("unreachable: %v", err)
	}
	if _, err := c.Lifecycle(ctx); err == nil {
		t.Fatal("unreachable lifecycle")
	}
}

func TestLifecycleTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxConfigBytes+1)))
	}))
	defer srv.Close()
	if _, err := newClient(t, srv.URL).Lifecycle(context.Background()); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
}

// decodePolicy returns the policy document's conditions.
func decodePolicy(t *testing.T, p Policy) (string, []any) {
	t.Helper()
	var b64 string
	for _, f := range p.Fields {
		if f.Name == "policy" {
			b64 = f.Value
		}
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Expiration string `json:"expiration"`
		Conditions []any  `json:"conditions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Expiration, doc.Conditions
}

// §4.2, DS5 (the signer's side): one quarantine key, the size range, the
// declared type, 10 minutes; the signature is SigV4's over the policy.
func TestPostPolicy(t *testing.T) {
	c := newClient(t, "http://minio.internal:9000")
	key := "quarantine/" + hosp + "/" + doc
	p, err := c.PostPolicy(PolicyRequest{Key: key, ContentType: "application/pdf", MaxBytes: 26214400}, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.URL != "http://minio.internal:9000/mhai-documents-test" || !p.ExpiresAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("%s %v", p.URL, p.ExpiresAt)
	}
	names := []string{}
	for _, f := range p.Fields {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "key,Content-Type,x-amz-algorithm,x-amz-credential,x-amz-date,policy,x-amz-signature" {
		t.Fatal(names)
	}
	exp, conds := decodePolicy(t, p)
	if exp != "2026-11-15T09:40:00.000Z" {
		t.Fatal(exp)
	}
	b, _ := json.Marshal(conds)
	want := `[{"bucket":"mhai-documents-test"},{"key":"` + key + `"},{"Content-Type":"application/pdf"},` +
		`["content-length-range",1,26214400],{"x-amz-algorithm":"AWS4-HMAC-SHA256"},` +
		`{"x-amz-credential":"TESTONLYDOCS/20261115/us-east-1/s3/aws4_request"},{"x-amz-date":"20261115T093000Z"}]`
	if string(b) != want {
		t.Fatalf("conditions\n%s\nwant\n%s", b, want)
	}
	// The signature is HMAC(signing key, base64 policy).
	m := hmac.New(sha256.New, signingKey(secret, "20261115", "us-east-1"))
	m.Write([]byte(p.Fields[5].Value))
	if hex.EncodeToString(m.Sum(nil)) != p.Fields[6].Value {
		t.Fatal("signature")
	}
	// The browser posts to the public endpoint; the signature doesn't change.
	pub, err := c.PostPolicy(PolicyRequest{Key: key, ContentType: "application/pdf", MaxBytes: 26214400}, "https://uploads.example.test")
	if err != nil || pub.URL != "https://uploads.example.test/mhai-documents-test" || pub.Fields[6] != p.Fields[6] {
		t.Fatalf("public %v %v", pub.URL, err)
	}
	for name, r := range map[string]PolicyRequest{
		"original":     {Key: "originals/" + hosp + "/" + doc, ContentType: "application/pdf", MaxBytes: 1},
		"page":         {Key: "pages/" + hosp + "/" + doc + "/1", ContentType: "application/pdf", MaxBytes: 1},
		"bad key":      {Key: "quarantine/" + hosp, ContentType: "application/pdf", MaxBytes: 1},
		"html":         {Key: key, ContentType: "text/html", MaxBytes: 1},
		"zero size":    {Key: key, ContentType: "application/pdf", MaxBytes: 0},
		"long ttl":     {Key: key, ContentType: "application/pdf", MaxBytes: 1, TTL: 11 * time.Minute},
		"negative ttl": {Key: key, ContentType: "application/pdf", MaxBytes: 1, TTL: -time.Minute},
	} {
		if _, err := c.PostPolicy(r, ""); !errors.Is(err, ErrPolicy) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := c.PostPolicy(PolicyRequest{Key: key, ContentType: "image/png", MaxBytes: 1}, "ftp://x"); !errors.Is(err, ErrConfig) {
		t.Fatalf("bad upload endpoint: %v", err)
	}
}

func TestCheckLifecycle(t *testing.T) {
	deployed, err := os.ReadFile("../../../../deploy/documents/bucket-lifecycle.xml")
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckLifecycle(deployed); err != nil {
		t.Fatalf("the deployed lifecycle: %v", err)
	}
	q := func(inner string) string {
		return `<Rule><Filter><Prefix>quarantine/</Prefix></Filter><Status>Enabled</Status>` + inner + `</Rule>`
	}
	nc := `<Rule><Filter></Filter><Status>Enabled</Status><NoncurrentVersionExpiration><NoncurrentDays>7</NoncurrentDays></NoncurrentVersionExpiration></Rule>`
	cfg := func(rules ...string) []byte {
		return []byte("<LifecycleConfiguration>" + strings.Join(rules, "") + "</LifecycleConfiguration>")
	}
	exp1 := `<Expiration><Days>1</Days></Expiration>`
	for name, tc := range map[string]struct {
		raw  []byte
		want error
	}{
		"both":               {cfg(q(exp1), nc), nil},
		"legacy prefix":      {cfg(`<Rule><Prefix>quarantine/</Prefix><Status>Enabled</Status>`+exp1+`</Rule>`, nc), nil},
		"not xml":            {[]byte("{}"), ErrLifecycleXML},
		"empty":              {cfg(), ErrNoQuarantineRule},
		"two days":           {cfg(q(`<Expiration><Days>2</Days></Expiration>`), nc), ErrNoQuarantineRule},
		"a date":             {cfg(q(`<Expiration><Date>2030-01-01T00:00:00Z</Date></Expiration>`), nc), ErrNoQuarantineRule},
		"disabled":           {cfg(strings.Replace(q(exp1), "Enabled", "Disabled", 1), nc), ErrNoQuarantineRule},
		"other prefix":       {cfg(strings.Replace(q(exp1), "quarantine/", "quarantine/x", 1), nc), ErrNoQuarantineRule},
		"whole bucket":       {cfg(`<Rule><Filter></Filter><Status>Enabled</Status>`+exp1+`</Rule>`, nc), ErrNoQuarantineRule},
		"tag filter":         {cfg(`<Rule><Filter><Tag><Key>k</Key><Value>v</Value></Tag></Filter><Status>Enabled</Status>`+exp1+`</Rule>`, nc), ErrNoQuarantineRule},
		"and filter":         {cfg(`<Rule><Filter><And><Prefix>quarantine/</Prefix></And></Filter><Status>Enabled</Status>`+exp1+`</Rule>`, nc), ErrNoQuarantineRule},
		"size filter":        {cfg(`<Rule><Filter><Prefix>quarantine/</Prefix><ObjectSizeGreaterThan>9</ObjectSizeGreaterThan></Filter><Status>Enabled</Status>`+exp1+`</Rule>`, nc), ErrNoQuarantineRule},
		"both prefixes":      {cfg(`<Rule><Prefix>quarantine/</Prefix><Filter><Prefix>x</Prefix></Filter><Status>Enabled</Status>`+exp1+`</Rule>`, nc), ErrNoQuarantineRule},
		"no noncurrent":      {cfg(q(exp1)), ErrNoNoncurrentRule},
		"noncurrent 30":      {cfg(q(exp1), strings.Replace(nc, ">7<", ">30<", 1)), ErrNoNoncurrentRule},
		"noncurrent keeps":   {cfg(q(exp1), strings.Replace(nc, "<NoncurrentDays>", "<NewerNoncurrentVersions>3</NewerNoncurrentVersions><NoncurrentDays>", 1)), ErrNoNoncurrentRule},
		"noncurrent on pfx":  {cfg(q(exp1), strings.Replace(nc, "<Filter></Filter>", "<Filter><Prefix>pages/</Prefix></Filter>", 1)), ErrNoNoncurrentRule},
		"noncurrent with q":  {cfg(q(exp1 + `<NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration>`)), ErrNoNoncurrentRule},
		"quarantine in same": {cfg(q(exp1), nc, q(`<Expiration><Days>3</Days></Expiration>`)), nil},
	} {
		if err := CheckLifecycle(tc.raw); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
}
