package s3

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

// POST policies (documents design §4.2, D6): the only way anything but the
// documents service writes to the bucket. Each is for exactly one
// quarantine key, a content-length range from 1 byte to the purpose's
// limit, the declared Content-Type and at most 10 minutes. A presigned PUT
// can't enforce a size range; a policy can.

// PolicyTTL is how long a policy is valid (§4.2).
const PolicyTTL = 10 * time.Minute

// quarantineKey is the only kind of key a policy may name.
var quarantineKey = regexp.MustCompile(`^quarantine/` + uuidRE + `/` + uuidRE + `$`)

// policyTypes are the content types a policy may name (§4.1).
var policyTypes = map[string]bool{"application/pdf": true, "image/png": true, "image/jpeg": true, "image/tiff": true}

// ErrPolicy: a policy for a key outside quarantine/, an unknown content
// type, a size limit below 1 byte or a lifetime outside (0, 10 minutes].
var ErrPolicy = errors.New("s3: policy refused")

// Field is one form field, in the order the browser posts them; the file
// goes last.
type Field struct{ Name, Value string }

// Policy is a signed POST policy.
type Policy struct {
	// URL is where the browser posts: <upload endpoint>/<bucket>.
	URL       string
	Fields    []Field
	ExpiresAt time.Time
}

// PolicyRequest is what one policy allows.
type PolicyRequest struct {
	Key         string
	ContentType string
	MaxBytes    int64
	// TTL is PolicyTTL when zero.
	TTL time.Duration
}

// PostPolicy signs a policy for r with c's credential. uploadEndpoint is
// the endpoint the browser reaches ("" for c's own).
func (c *Client) PostPolicy(r PolicyRequest, uploadEndpoint string) (Policy, error) {
	ttl := r.TTL
	if ttl == 0 {
		ttl = PolicyTTL
	}
	if !quarantineKey.MatchString(r.Key) || !policyTypes[r.ContentType] || r.MaxBytes < 1 || ttl < 0 || ttl > PolicyTTL {
		return Policy{}, ErrPolicy
	}
	base := c.endpoint
	if uploadEndpoint != "" {
		u, err := ParseEndpoint(uploadEndpoint)
		if err != nil {
			return Policy{}, err
		}
		base = u
	}
	now := c.now().UTC()
	exp := now.Add(ttl)
	cred := c.creds.AccessKeyID + "/" + scope(shortDate(now), c.region)
	date := amzDate(now)
	doc := map[string]any{
		"expiration": exp.Format("2006-01-02T15:04:05.000Z"),
		"conditions": []any{
			map[string]string{"bucket": c.bucket},
			map[string]string{"key": r.Key},
			map[string]string{"Content-Type": r.ContentType},
			[]any{"content-length-range", 1, r.MaxBytes},
			map[string]string{"x-amz-algorithm": Algorithm},
			map[string]string{"x-amz-credential": cred},
			map[string]string{"x-amz-date": date},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return Policy{}, ErrPolicy
	}
	b64 := base64.StdEncoding.EncodeToString(raw)
	sig := hex.EncodeToString(hmacSHA256(signingKey(c.creds.Secret, shortDate(now), c.region), b64))
	u := *base
	u.Path = "/" + c.bucket
	return Policy{
		URL: u.String(),
		Fields: []Field{
			{"key", r.Key},
			{"Content-Type", r.ContentType},
			{"x-amz-algorithm", Algorithm},
			{"x-amz-credential", cred},
			{"x-amz-date", date},
			{"policy", b64},
			{"x-amz-signature", sig},
		},
		ExpiresAt: exp,
	}, nil
}
