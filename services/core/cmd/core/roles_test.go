package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// The roles command refuses any malformed argument before connecting, and
// never echoes an email.
func TestRolesCLIRefusesBeforeConnecting(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"ada@example.test"},
		{"ada@example.test", "--add", "analyst"},
		{"ada@example.test", "--add", "analyst", "--by"},
		{"ada@example.test", "--add", "hospital_admin", "--by", "admin@example.test"},
		{"ada@example.test", "--add", "platform_admin", "--by", "admin@example.test"},
		{"ada@example.test", "--add", "analyst", "--remove", "analyst"},
		{"ada@example.test", "--by", "admin@example.test", "--by", "admin@example.test"},
		{"ada@example.test", "--add", "analyst", "--by", "--add"},
		{"--add", "analyst", "--by", "admin@example.test", "x"},
		{"not-an-email", "--add", "analyst", "--by", "admin@example.test"},
		{"ada@example.test", "--add", "analyst", "--by", "not-an-email"},
		{"ada@example.test", "--add", "analyst", "--by", "admin@example.test", "--x"},
	} {
		var out bytes.Buffer
		err := usersRoles(context.Background(), args, "", &out, nil)
		if !errors.Is(err, errRolesArgs) {
			t.Errorf("%v: not the usage refusal (reached the database step?): %v", args, err)
			continue
		}
		if strings.Contains(err.Error(), "ada@") || strings.Contains(err.Error(), "admin@") || out.Len() != 0 {
			t.Errorf("%v: echoed input: %v %q", args, err, out.String())
		}
	}
}
