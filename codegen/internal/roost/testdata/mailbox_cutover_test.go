package controllers

import (
	"context"
	"errors"
	gamelogin "example.com/studio/mailcutover/game/modules/login"
	"example.com/studio/mailcutover/protocol/pb"
	"github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
	account "github.com/tjbdwanghaibo/roost-core/service/account"
	mail "github.com/tjbdwanghaibo/roost-core/service/mail"
	"strconv"
	"testing"
	"time"
)

type cutoverClient struct {
	mail.Mail
	failures, pulls int
}

func (c *cutoverClient) PullBroadcasts(ctx context.Context, player int64, p mail.BroadcastProfile) (mail.Summary, error) {
	c.pulls++
	if c.failures > 0 {
		c.failures--
		return mail.Summary{}, errors.New("injected: pull not delivered")
	}
	return c.Mail.PullBroadcasts(ctx, player, p)
}

type cutoverSession struct {
	gateway.Session
	principal gateway.Principal
}

func (s cutoverSession) Principal() gateway.Principal { return s.principal }

func TestGeneratedMailboxCutover(t *testing.T) {
	t.Cleanup(gamelogin.KeepOnlyRegistrationsForTest("mailbox", "rehome"))
	ctx := context.Background()
	w := newRehomeWorld(t, rehomeSourceSID, rehomeTargetSID)
	src, dst := w.server(rehomeSourceSID), w.server(rehomeTargetSID)
	mailer := mailboxRehomeService(t)
	client := &cutoverClient{Mail: mailer, failures: 2}
	if err := src.registry.Register(mail.CapabilityName, mailer); err != nil {
		t.Fatal(err)
	}
	if err := dst.registry.Register(mail.CapabilityName, client); err != nil {
		t.Fatal(err)
	}
	claims := func() map[string]string {
		m := withRehomeClaims(w.binding())
		m[account.RoleCreatedAtClaim] = strconv.FormatInt(time.UnixMilli(w.now.Load()).Add(-24*time.Hour).Unix(), 10)
		return m
	}
	loads := 0
	enter := func(s *rehomeServer) {
		if err := w.enter(s, claims(), &loads); err != nil {
			t.Fatal(err)
		}
	}
	send := func(sid int32, key string) string {
		e, err := mailer.Send(ctx, mail.SendRequest{Audience: mail.AudienceBroadcast, Scope: mail.ServerScope(sid), RequestID: key, Subject: key, Body: key, ExpiresInSeconds: 3600})
		if err != nil {
			t.Fatal(err)
		}
		return e.ID
	}
	enter(src)
	old := send(rehomeSourceSID, "old")
	early := send(rehomeTargetSID, "early")
	if r := w.apply(src, rehomeTargetSID); r.Code != 0 {
		t.Fatalf("apply: %+v", r)
	}
	w.settled(src)
	if w.binding().ServingServerID != rehomeTargetSID {
		t.Fatal("not committed")
	}
	enter(dst)
	if client.pulls != 1 || loads != 2 {
		t.Fatalf("enter pulls=%d loads=%d", client.pulls, loads)
	}
	late := send(rehomeSourceSID, "late")
	fresh := send(rehomeTargetSID, "fresh")
	session := mailboxAs(w.playerID)
	session.Session = cutoverSession{principal: gateway.Principal{PlayerID: w.playerID, SessionID: "dst", Claims: claims()}}
	list := func() *pb.MailboxListResponse {
		r, err := dst.controller.HandleMailboxList(session, &pb.MailboxListRequest{})
		if err != nil || r == nil {
			t.Fatalf("list: %+v %v", r, err)
		}
		return r
	}
	if r := list(); r.Code == 0 || client.pulls != 2 {
		t.Fatalf("list while pull still unavailable: %+v pulls=%d", r, client.pulls)
	}
	box, _, err := mailer.Mailbox(ctx, w.playerID)
	if err != nil || box.BroadcastProfile != nil || box.BroadcastWatermark != 2 {
		t.Fatalf("failed pulls advanced cutoff: %+v %v", box, err)
	}
	check := func() {
		r := list()
		got := map[string]bool{}
		for _, m := range r.Mails {
			got[m.MailID] = true
		}
		if r.Code != 0 || len(r.Mails) != 2 || !got[old] || !got[fresh] || got[early] || got[late] {
			t.Fatalf("wrong side of cutover: %+v", r)
		}
	}
	check()
	if client.pulls != 3 {
		t.Fatalf("repair pulls=%d", client.pulls)
	}
	check()
	if client.pulls != 3 {
		t.Fatal("normal list rewrote scope")
	}
	enter(dst)
	check()
}
