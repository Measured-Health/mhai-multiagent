package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/codes"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/rules"
)

// loadRules loads the rule set file and checks its units against the
// normaliser's conversions: an analyte the conversions put in another unit
// refuses the start (a threshold would compare against the wrong unit). It
// returns the analyte codes the conversions don't list yet: their numeric
// rules can't fire until they do (their source flags still do).
func loadRules(path string, maps *codes.Maps, development bool) (*rules.RuleSet, []string, error) {
	rs, err := rules.Load(path, development)
	if err != nil {
		if errors.Is(err, rules.ErrSynthetic) {
			return nil, nil, errors.New("EMR_RULE_SET_FILE: a synthetic rule set is for development only")
		}
		return nil, nil, errors.New("EMR_RULE_SET_FILE: the rule set is missing or not valid (emr rules check <file>)")
	}
	missing, err := rs.UnitsMissing(maps)
	if err != nil {
		return nil, nil, errors.New("EMR_RULE_SET_FILE: an analyte's unit differs from the conversions' canonical unit")
	}
	return rs, missing, nil
}

// rulesCheck is "emr rules check <file>": it loads the rule set as the
// service would (synthetic sets in development only) and prints what a
// platform admin activates for a hospital: the version and the SHA-256.
func rulesCheck(path string, getenv func(string) string, out io.Writer) error {
	rs, err := rules.Load(path, getenv("APP_ENV") == "development")
	if err != nil {
		return err
	}
	sha := rs.SHA256()
	_, err = fmt.Fprintf(out, "version: %s\nstatus: %s\nclinical_signoff: %s\nrules: %d\nsha256: %s\n",
		rs.Version(), rs.Status(), rs.Signoff(), len(rs.RuleIDs()), hex.EncodeToString(sha[:]))
	return err
}
