package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// AnalysisKeyLen is the length of each analysis key, in bytes.
const AnalysisKeyLen = 32

// AnalysisKeys are Patient Analysis's four keys (analysis plan Task 3):
// the patient-reference HMAC and encryption keys, the drill-key HMAC key
// and the purpose-note key. Each comes from a file holding 32 random bytes
// in base64. They never print.
type AnalysisKeys struct {
	RefHMAC, RefEnc, Drill, Note []byte
}

// Set reports whether the keys were configured (all four, or none).
func (k AnalysisKeys) Set() bool { return k.RefHMAC != nil }

// String and GoString keep the bytes out of %v, %+v, %#v and logs.
func (k AnalysisKeys) String() string {
	if !k.Set() {
		return "AnalysisKeys{unset}"
	}
	return "AnalysisKeys{redacted}"
}

func (k AnalysisKeys) GoString() string { return k.String() }

// analysisKeyVars name the files, in the order of AnalysisKeys' fields.
var analysisKeyVars = [4]string{"ANALYSIS_REF_HMAC_KEY_FILE", "ANALYSIS_REF_ENC_KEY_FILE", "ANALYSIS_DRILL_KEY_FILE", "ANALYSIS_NOTE_KEY_FILE"}

// loadAnalysisKeys reads the four key files: none set is no keys (Analysis
// can't ingest a signed-off hospital, and fails closed there); otherwise
// all four, each refused when missing, readable by others, a placeholder,
// not 32 bytes of base64, or equal to another. Errors name the variable,
// never the path or the content.
func loadAnalysisKeys(getenv func(string) string) (AnalysisKeys, error) {
	var paths [4]string
	set := 0
	for i, v := range analysisKeyVars {
		paths[i] = strings.TrimSpace(getenv(v))
		if paths[i] != "" {
			set++
		}
	}
	if set == 0 {
		return AnalysisKeys{}, nil
	}
	if set != len(paths) {
		return AnalysisKeys{}, errors.New("ANALYSIS_REF_HMAC_KEY_FILE, ANALYSIS_REF_ENC_KEY_FILE, ANALYSIS_DRILL_KEY_FILE and ANALYSIS_NOTE_KEY_FILE must be set together")
	}
	var keys [4][]byte
	var errs []error
	for i, v := range analysisKeyVars {
		k, err := readAnalysisKey(v, paths[i])
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for j := 0; j < i; j++ {
			if keys[j] != nil && bytes.Equal(keys[j], k) {
				errs = append(errs, fmt.Errorf("%s must be a different key from %s", v, analysisKeyVars[j]))
			}
		}
		keys[i] = k
	}
	if err := errors.Join(errs...); err != nil {
		return AnalysisKeys{}, err
	}
	return AnalysisKeys{RefHMAC: keys[0], RefEnc: keys[1], Drill: keys[2], Note: keys[3]}, nil
}

func readAnalysisKey(name, path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s does not name a readable key file", name)
	}
	if st.Mode().Perm()&0o007 != 0 {
		return nil, fmt.Errorf("%s is readable by other users; make it 0600 or 0640", name)
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- the operator's own key file
	if err != nil {
		return nil, fmt.Errorf("%s does not name a readable key file", name)
	}
	text := strings.TrimSpace(string(raw))
	if err := checkPlaceholder(name, text); err != nil {
		return nil, err
	}
	k, err := base64.StdEncoding.DecodeString(text)
	if err != nil || len(k) != AnalysisKeyLen {
		return nil, fmt.Errorf("%s must hold %d random bytes in base64", name, AnalysisKeyLen)
	}
	return k, nil
}
