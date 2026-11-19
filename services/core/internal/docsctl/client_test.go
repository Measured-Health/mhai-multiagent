package docsctl

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/docspb"
)

var hospital = uuid.MustParse("22222222-2222-4222-8222-222222222222")

// Core's own check comes before anything is sent (the client has no
// connection here, so a call that got past it would be ErrMisuse for that).
func TestRequestRefusesBeforeSending(t *testing.T) {
	c := &Client{}
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		hospital  uuid.UUID
		rid, op   string
		req, resp proto.Message
	}{
		{"unknown op", hospital, "rid-1", "nope", &docspb.HealthRequest{}, &docspb.HealthReply{}},
		{"wrong request type", hospital, "rid-1", "page_image", &docspb.HealthRequest{}, &docspb.PageImageReply{}},
		{"wrong reply type", hospital, "rid-1", "presign_upload", &docspb.PresignUploadRequest{}, &docspb.HealthReply{}},
		{"nil request", hospital, "rid-1", "health", nil, &docspb.HealthReply{}},
		{"no hospital", uuid.Nil, "rid-1", "health", &docspb.HealthRequest{}, &docspb.HealthReply{}},
		{"no request id", hospital, "", "health", &docspb.HealthRequest{}, &docspb.HealthReply{}},
		{"request id with a space", hospital, "has space", "health", &docspb.HealthRequest{}, &docspb.HealthReply{}},
		{"long request id", hospital, strings.Repeat("r", 129), "health", &docspb.HealthRequest{}, &docspb.HealthReply{}},
		{"no connection", hospital, "rid-1", "health", &docspb.HealthRequest{}, &docspb.HealthReply{}},
	} {
		if err := c.Request(ctx, tc.hospital, tc.rid, tc.op, tc.req, tc.resp); !errors.Is(err, ErrMisuse) {
			t.Errorf("%s: %v, want ErrMisuse", tc.name, err)
		}
	}
}

func TestValidRID(t *testing.T) {
	for s, want := range map[string]bool{"rid-1": true, "": false, "a b": false, "\x7f": false, "é": false,
		strings.Repeat("r", 128): true, strings.Repeat("r", 129): false} {
		if ValidRID(s) != want {
			t.Errorf("ValidRID(%q) != %v", s, want)
		}
	}
	if reasonOf(ErrNoResponders) != "no_responders" || reasonOf(ErrTimeout) != "timeout" {
		t.Fatal("reasonOf")
	}
}
