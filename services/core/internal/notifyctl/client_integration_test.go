//go:build integration

package notifyctl

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/delivery"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/notifypb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
)

// On the real NATS config: core's send_link carries a request token the
// notifications service verifies (aud notifications, the subject, the
// body, core's inbox, the hospital), the link only in the body; the
// service's answer decides Send's error. No service answering is
// ErrNoResponders, and a refused (unanswered) request a timeout.
func TestSendLinkOverNATS(t *testing.T) {
	ctx := context.Background()
	n, stop, err := testutil.NewNATS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer := reqtoken.Signer{Key: priv, KeyID: reqtoken.KeyID(priv.Public().(ed25519.PublicKey))}
	gw, _, err := bus.Connect(ctx, n.GatewayURL, "", bus.GatewayInboxPrefix, bus.TLS{})
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	c := &Client{NC: gw, Signer: signer}
	s := Sender{Client: c}
	h, user := uuid.New(), uuid.New()
	link := "https://app.example.test/reset-password?token=" + uuid.NewString()
	msg := delivery.Message{Kind: delivery.PasswordReset, HospitalID: h, Recipient: user, Link: link, ExpiresAt: time.Now().Add(30 * time.Minute)}

	// No service yet.
	if err := s.Send(ctx, msg); !errors.Is(err, ErrNoResponders) {
		t.Fatalf("no service: %v", err)
	}
	svc, err := nats.Connect(n.NotificationsURL, nats.CustomInboxPrefix(bus.WorkerInbox("notifications")))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	answer := "accepted"
	var got notifypb.SendLinkRequest
	var claims reqtoken.Claims
	sub, err := svc.QueueSubscribe("notify.v1.ctl.>", "notifications", func(m *nats.Msg) {
		cl, err := reqtoken.VerifyAt(m.Header.Get(HeaderToken), signer.PublicKeys(),
			reqtoken.Expect{Aud: "notifications", Subject: m.Subject, Kind: reqtoken.KindReq}, m.Data, time.Now())
		if err != nil || reqtoken.CheckReply(cl, m.Reply, false) != nil {
			return // refused: never answered
		}
		claims = cl
		_ = proto.Unmarshal(m.Data, &got)
		if answer == "" {
			return
		}
		b, _ := proto.Marshal(&notifypb.SendLinkReply{Outcome: answer})
		out := nats.NewMsg(m.Reply)
		out.Data = b
		out.Header.Set(HeaderSchema, "mhai.notify.v1.SendLinkReply")
		out.Header.Set(HeaderRequestID, cl.Rid)
		_ = m.RespondMsg(out)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	_ = svc.Flush()
	if err := s.Send(ctx, msg); err != nil {
		t.Fatalf("accepted: %v", err)
	}
	if claims.Aud != "notifications" || claims.Sub != reqtoken.SvcCore || claims.Hid != h.String() ||
		claims.Subject != "notify.v1.ctl.send_link" || got.GetLink() != link || got.GetUserId() != user.String() ||
		got.GetPurpose() != "password_reset" || got.GetExpiresAt() != msg.ExpiresAt.Unix() {
		t.Fatalf("claims %+v request %v", claims, &got)
	}
	// A platform team member's invite: send_platform_link, no hospital in
	// the token, on the real NATS permissions.
	pm := delivery.Message{Kind: delivery.Invite, Recipient: user, Link: link, ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.Send(ctx, pm); err != nil {
		t.Fatalf("platform invite: %v", err)
	}
	if claims.Hid != "" || claims.Subject != "notify.v1.ctl.send_platform_link" || got.GetPurpose() != "invite" {
		t.Fatalf("platform invite claims %+v request %v", claims, &got)
	}
	// No hospital is only for an invite.
	pr := msg
	pr.HospitalID = uuid.Nil
	if err := s.Send(ctx, pr); !errors.Is(err, ErrMisuse) {
		t.Fatalf("a reset with no hospital: %v", err)
	}
	answer = "no_recipient"
	if err := s.Send(ctx, msg); !errors.Is(err, ErrNotSent) {
		t.Fatalf("no_recipient: %v", err)
	}
	answer = ""
	tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.Send(tctx, msg); !errors.Is(err, ErrTimeout) {
		t.Fatalf("unanswered: %v", err)
	}
}
