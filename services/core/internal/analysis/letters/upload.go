package letters

import (
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// MaxCovered is the most people one letter covers, staff and platform
// admins together (contract §10, COVER_MAX).
const MaxCovered = 20

// maxField bounds a text field of the upload.
const maxField = 512

// uploadIn is a parsed upload. The file's bytes are hashed and dropped;
// nothing else reads them.
type uploadIn struct {
	file                  []byte
	signedBy, signerTitle string
	letterDate, endDate   time.Time
	covered               []uuid.UUID
	coveredPlatform       []string
	renews                *uuid.UUID
	mfaCode, backupCode   string
}

var (
	// emailShape is a plausible address, as written in the letter; the
	// database decides whether it is an eligible platform admin's.
	emailShape = regexp.MustCompile(`^[^@\s\x00-\x1f\x7f]{1,64}@[^@\s\x00-\x1f\x7f]{1,189}$`)
	codeShape  = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

// single are the fields that may appear once; covered and
// covered_platform may repeat. Nothing else is accepted (T19: the
// hospital, the uploader, a status or a version are the server's).
var single = map[string]bool{"file": true, "signed_by": true, "signer_title": true, "letter_date": true,
	"end_date": true, "renews": true, "mfa_code": true, "backup_code": true}

// parseUpload reads a multipart upload within the API's body limit. It
// returns a problem code on any refusal: file_type (not multipart),
// file_too_large (past the limit), validation_failed (anything else).
func parseUpload(r *http.Request) (uploadIn, string) {
	var in uploadIn
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return in, "file_type"
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	seen := map[string]bool{}
	text := map[string]string{}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return in, tooLargeOr(err)
		}
		name := part.FormName()
		repeat := name == "covered" || name == "covered_platform"
		if !repeat && (!single[name] || seen[name]) {
			_ = part.Close()
			return in, "validation_failed"
		}
		seen[name] = true
		if name == "file" {
			b, err := io.ReadAll(part)
			_ = part.Close()
			if err != nil {
				return in, tooLargeOr(err)
			}
			if len(b) == 0 {
				return in, "validation_failed"
			}
			in.file = b
			continue
		}
		b, err := io.ReadAll(io.LimitReader(part, maxField+1))
		_ = part.Close()
		if err != nil {
			return in, tooLargeOr(err)
		}
		v := string(b)
		if len(b) > maxField || !utf8.ValidString(v) || v == "" {
			return in, "validation_failed"
		}
		switch name {
		case "covered":
			u, ok := canonicalID(v)
			if !ok {
				return in, "validation_failed"
			}
			in.covered = append(in.covered, u)
		case "covered_platform":
			if !emailShape.MatchString(v) || len(v) > 254 {
				return in, "validation_failed"
			}
			in.coveredPlatform = append(in.coveredPlatform, v)
		default:
			text[name] = v
		}
	}
	if in.file == nil {
		return in, "validation_failed"
	}
	for _, f := range []string{"signed_by", "signer_title"} {
		v := text[f]
		if !textShape.MatchString(v) || utf8.RuneCountInString(v) > 120 || v != trimmed(v) {
			return in, "validation_failed"
		}
	}
	in.signedBy, in.signerTitle = text["signed_by"], text["signer_title"]
	var ok bool
	if in.letterDate, ok = date(text["letter_date"]); !ok {
		return in, "validation_failed"
	}
	if in.endDate, ok = date(text["end_date"]); !ok {
		return in, "validation_failed"
	}
	if v, set := text["renews"]; set {
		u, ok := canonicalID(v)
		if !ok {
			return in, "validation_failed"
		}
		in.renews = &u
	}
	for _, f := range []string{"mfa_code", "backup_code"} {
		if v, set := text[f]; set && !codeShape.MatchString(v) {
			return in, "validation_failed"
		}
	}
	in.mfaCode, in.backupCode = text["mfa_code"], text["backup_code"]
	n := len(in.covered) + len(in.coveredPlatform)
	if n < 1 || n > MaxCovered {
		return in, "validation_failed"
	}
	ids := map[uuid.UUID]bool{}
	for _, u := range in.covered {
		if ids[u] {
			return in, "validation_failed"
		}
		ids[u] = true
	}
	emails := map[string]bool{}
	for _, e := range in.coveredPlatform {
		k := lower(e)
		if emails[k] {
			return in, "validation_failed"
		}
		emails[k] = true
	}
	return in, ""
}

// tooLargeOr is file_too_large for a body past the limit, else
// validation_failed.
func tooLargeOr(err error) string {
	var mb *http.MaxBytesError
	if errors.As(err, &mb) {
		return "file_too_large"
	}
	return "validation_failed"
}

func date(s string) (time.Time, bool) {
	t, err := time.Parse(time.DateOnly, s)
	return t, err == nil && t.Format(time.DateOnly) == s
}

func trimmed(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
