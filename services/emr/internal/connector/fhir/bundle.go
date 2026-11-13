package fhir

import (
	"encoding/json"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// bundle is a searchset Bundle, read for its links and entries only. Each
// entry's resource stays raw until it is read into its own allow-listed
// struct (map.go).
type bundle struct {
	ResourceType string `json:"resourceType"`
	Type         string `json:"type"`
	Link         []struct {
		Relation string `json:"relation"`
		URL      string `json:"url"`
	} `json:"link"`
	Entry []struct {
		Resource json.RawMessage `json:"resource"`
		Search   *struct {
			Mode string `json:"mode"`
		} `json:"search"`
	} `json:"entry"`
}

// next is the bundle's next link, or "".
func (b bundle) next() string {
	for _, l := range b.Link {
		if l.Relation == "next" {
			return l.URL
		}
	}
	return ""
}

// idShape is a FHIR id (R4 id datatype).
var idShape = regexp.MustCompile(`^[A-Za-z0-9.-]{1,64}$`)

// underBase reports whether next is an https URL on the base address's
// host and port, under its path, with no userinfo, fragment or dot
// segment (design §5.1, A7: a next link elsewhere is never followed).
func underBase(next, base string) bool {
	n, err1 := url.Parse(next)
	b, err2 := url.Parse(base)
	if err1 != nil || err2 != nil || n.Scheme != "https" || n.User != nil || n.Fragment != "" || n.Opaque != "" ||
		!strings.EqualFold(n.Hostname(), b.Hostname()) || portOf(n) != portOf(b) {
		return false
	}
	ep := n.EscapedPath()
	low := strings.ToLower(ep)
	if strings.Contains(low, "%2e") || strings.Contains(low, "%2f") || strings.Contains(low, "%5c") || strings.Contains(ep, "\\") {
		return false
	}
	if path.Clean(n.Path) != n.Path {
		return false
	}
	bp := strings.TrimSuffix(b.Path, "/")
	return strings.HasPrefix(n.Path, bp+"/")
}

func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	return "443"
}
