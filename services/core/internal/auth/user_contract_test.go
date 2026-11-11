package auth

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The User schema in openapi.yaml and the login reply's user (userView)
// have the same fields, all required, with features exactly
// {analysis, named_access} (analysis design §6.5).
func TestOpenAPIUserMatchesView(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	type schema struct {
		Required   []string          `yaml:"required"`
		Properties map[string]schema `yaml:"properties"`
	}
	var spec struct {
		Components struct {
			Schemas map[string]schema `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	user := spec.Components.Schemas["User"]
	var tags []string
	ty := reflect.TypeOf(userView{})
	for i := 0; i < ty.NumField(); i++ {
		tags = append(tags, strings.Split(ty.Field(i).Tag.Get("json"), ",")[0])
	}
	var props []string
	for k := range user.Properties {
		props = append(props, k)
	}
	slices.Sort(tags)
	slices.Sort(props)
	req := slices.Clone(user.Required)
	slices.Sort(req)
	if !slices.Equal(tags, props) || !slices.Equal(tags, req) {
		t.Fatalf("userView %v\nUser properties %v\nrequired %v", tags, props, req)
	}
	f := user.Properties["features"]
	var fp []string
	for k := range f.Properties {
		fp = append(fp, k)
	}
	slices.Sort(fp)
	fr := slices.Clone(f.Required)
	slices.Sort(fr)
	if strings.Join(fp, ",") != "analysis,named_access" || strings.Join(fr, ",") != "analysis,named_access" {
		t.Fatalf("User.features %v required %v", fp, fr)
	}
}
