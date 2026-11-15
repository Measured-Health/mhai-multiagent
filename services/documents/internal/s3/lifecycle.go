package s3

import (
	"encoding/xml"
	"errors"
)

// The bucket's lifecycle (documents design §5.4, §6.1;
// deploy/documents/bucket-lifecycle.xml): quarantine/ is the only place
// unwrapped bytes are ever stored, so a lifecycle rule must delete it
// within a day whatever the service does, and versions that are no longer
// current must go within 7 days so a deletion completes. The service is
// not ready to take an upload unless both rules are in force.

const (
	// MaxQuarantineDays is the longest the quarantine rule may keep an
	// object.
	MaxQuarantineDays = 1
	// MaxNoncurrentDays is the longest a non-current version may stay.
	MaxNoncurrentDays = 7
)

var (
	// ErrNoQuarantineRule: no enabled rule expires quarantine/ within a day.
	ErrNoQuarantineRule = errors.New("s3: no lifecycle rule deletes quarantine/ within a day")
	// ErrNoNoncurrentRule: no enabled whole-bucket rule expires non-current
	// versions within 7 days.
	ErrNoNoncurrentRule = errors.New("s3: no lifecycle rule expires non-current versions within 7 days")
	// ErrLifecycleXML: the configuration doesn't parse.
	ErrLifecycleXML = errors.New("s3: the lifecycle configuration is not valid")
)

type lcFilter struct {
	Prefix *string   `xml:"Prefix"`
	Tag    *struct{} `xml:"Tag"`
	And    *struct{} `xml:"And"`
	// Size filters narrow a rule too.
	GreaterThan *string `xml:"ObjectSizeGreaterThan"`
	LessThan    *string `xml:"ObjectSizeLessThan"`
}

type lcRule struct {
	Status     string    `xml:"Status"`
	Prefix     *string   `xml:"Prefix"` // the older form, outside Filter
	Filter     *lcFilter `xml:"Filter"`
	Expiration *struct {
		Days int     `xml:"Days"`
		Date *string `xml:"Date"`
	} `xml:"Expiration"`
	Noncurrent *struct {
		NoncurrentDays int  `xml:"NoncurrentDays"`
		Newer          *int `xml:"NewerNoncurrentVersions"`
	} `xml:"NoncurrentVersionExpiration"`
}

type lcConfig struct {
	XMLName xml.Name `xml:"LifecycleConfiguration"`
	Rules   []lcRule `xml:"Rule"`
}

// prefix is the rule's object prefix, and false if the rule is narrowed by
// anything else (tags, sizes, And), which the check can't rely on.
func (r lcRule) prefix() (string, bool) {
	p := ""
	if r.Prefix != nil {
		p = *r.Prefix
	}
	if f := r.Filter; f != nil {
		if f.Tag != nil || f.And != nil || f.GreaterThan != nil || f.LessThan != nil {
			return "", false
		}
		if f.Prefix != nil {
			if p != "" && *f.Prefix != "" {
				return "", false
			}
			p += *f.Prefix
		}
	}
	return p, true
}

// CheckLifecycle reports whether the configuration deletes quarantine/
// within a day and non-current versions within 7 days.
func CheckLifecycle(raw []byte) error {
	var c lcConfig
	if err := xml.Unmarshal(raw, &c); err != nil {
		return ErrLifecycleXML
	}
	quarantine, noncurrent := false, false
	for _, r := range c.Rules {
		if r.Status != "Enabled" {
			continue
		}
		p, ok := r.prefix()
		if !ok {
			continue
		}
		// Only a rule on quarantine/ itself counts: an expiry on the whole
		// bucket would delete originals and pages too.
		if p == "quarantine/" && r.Expiration != nil && r.Expiration.Date == nil &&
			r.Expiration.Days >= 1 && r.Expiration.Days <= MaxQuarantineDays {
			quarantine = true
		}
		// NewerNoncurrentVersions keeps some versions past the days.
		if p == "" && r.Noncurrent != nil && r.Noncurrent.Newer == nil &&
			r.Noncurrent.NoncurrentDays >= 1 && r.Noncurrent.NoncurrentDays <= MaxNoncurrentDays {
			noncurrent = true
		}
	}
	switch {
	case !quarantine:
		return ErrNoQuarantineRule
	case !noncurrent:
		return ErrNoNoncurrentRule
	}
	return nil
}
