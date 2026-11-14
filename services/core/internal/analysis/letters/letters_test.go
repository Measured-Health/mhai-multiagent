package letters

import (
	"bytes"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
)

func multipartReq(t *testing.T, fields [][2]string, file []byte, limit int64) *http.Request {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	if file != nil {
		fw, _ := mw.CreateFormFile("file", "x.pdf")
		_, _ = fw.Write(file)
	}
	for _, kv := range fields {
		_ = mw.WriteField(kv[0], kv[1])
	}
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/", &b)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if limit > 0 {
		r.Body = http.MaxBytesReader(httptest.NewRecorder(), r.Body, limit)
	}
	return r
}

func TestParseUpload(t *testing.T) {
	staff := uuid.NewString()
	good := [][2]string{{"signed_by", "Synthetic Signer"}, {"signer_title", "Director"}, {"letter_date", "2026-11-01"},
		{"end_date", "2027-05-01"}, {"covered", staff}, {"covered_platform", "pa@measured-health.example"}, {"mfa_code", "123456"}}
	in, code := parseUpload(multipartReq(t, good, []byte("x"), 0))
	if code != "" || in.signedBy != "Synthetic Signer" || len(in.covered) != 1 || len(in.coveredPlatform) != 1 || in.mfaCode != "123456" {
		t.Fatalf("good upload: %q %+v", code, in)
	}
	for _, c := range []struct {
		name   string
		fields [][2]string
		file   []byte
		limit  int64
		want   string
	}{
		{"no file", good, nil, 0, "validation_failed"},
		{"an empty file", good, []byte{}, 0, "validation_failed"},
		{"too large", good, bytes.Repeat([]byte("x"), 2048), 1024, "file_too_large"},
		{"an unknown field", append(good, [2]string{"hospital_id", staff}), []byte("x"), 0, "validation_failed"},
		{"a field twice", append(good, [2]string{"end_date", "2027-05-02"}), []byte("x"), 0, "validation_failed"},
		{"a bad id", append(good[:4:4], [2]string{"covered", "nope"}), []byte("x"), 0, "validation_failed"},
		{"a bad email", append(good[:5:5], [2]string{"covered_platform", "not an email"}), []byte("x"), 0, "validation_failed"},
		{"a padded signer", append([][2]string{{"signed_by", " Padded"}}, good[1:]...), []byte("x"), 0, "validation_failed"},
		{"no people", good[:4], []byte("x"), 0, "validation_failed"},
	} {
		if _, got := parseUpload(multipartReq(t, c.fields, c.file, c.limit)); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/json")
	if _, got := parseUpload(r); got != "file_type" {
		t.Errorf("JSON: %q", got)
	}
}

func TestFixturePagesArePNG(t *testing.T) {
	for n := 1; n <= DemoPages; n++ {
		b := fixturePage(n)
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil || img.Bounds().Dx() != 595 {
			t.Fatalf("page %d: %v", n, err)
		}
		if !bytes.Equal(b, fixturePage(n)) {
			t.Errorf("page %d is not the same bytes twice", n)
		}
	}
	if !bytes.Equal(fixturePage(1), fixturePage(1)) || bytes.Equal(fixturePage(1), fixturePage(2)) {
		t.Error("pages are not distinct")
	}
	if fixturePage(0) != nil || fixturePage(DemoPages+1) != nil {
		t.Error("a page outside the fixture")
	}
}

func TestRules(t *testing.T) {
	if !ReadRule.NeedsMFA || WriteRule.Write != true || !WriteRule.StepUp || len(WriteRule.AnyRole) != 1 ||
		WriteRule.AnyRole[0] != authz.RoleHospitalAdmin || ReadRule.AnyRole[0] != authz.RoleHospitalAdmin {
		t.Errorf("rules: %+v %+v", ReadRule, WriteRule)
	}
}
