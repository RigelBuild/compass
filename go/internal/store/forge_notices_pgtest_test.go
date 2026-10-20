//go:build pgtest

package store

import (
	"errors"
	"testing"
)

func TestRecordNoticeIsIdempotentOnRequestKey(t *testing.T) {
	s := newTestStore(t)
	from, _ := seedAgent(t, s, "notice-idempotent-from")
	to, _ := seedAgent(t, s, "notice-idempotent-to")
	input := Notice{FromAccountID: from, ToAccountID: to, RequestKey: "RIG-5881:notice:base:new-head", Text: "first notice"}
	first, err := s.RecordNotice(t.Context(), input)
	if err != nil {
		t.Fatalf("RecordNotice: %v", err)
	}
	input.Text = "different retry text"
	retry, err := s.RecordNotice(t.Context(), input)
	if err != nil {
		t.Fatalf("RecordNotice retry: %v", err)
	}
	if retry != first {
		t.Fatalf("retry = %+v, want original notice %+v", retry, first)
	}
}

func TestUndeliveredNoticesSkipsDeliveredRows(t *testing.T) {
	s := newTestStore(t)
	from, _ := seedAgent(t, s, "notice-delivered-from")
	to, _ := seedAgent(t, s, "notice-delivered-to")
	first, err := s.RecordNotice(t.Context(), Notice{
		FromAccountID: from, ToAccountID: to, RequestKey: "RIG-5881:notice:delivered", Text: "delivered",
	})
	if err != nil {
		t.Fatalf("RecordNotice(delivered): %v", err)
	}
	pending, err := s.RecordNotice(t.Context(), Notice{
		FromAccountID: from, ToAccountID: to, RequestKey: "RIG-5881:notice:pending", Text: "pending",
	})
	if err != nil {
		t.Fatalf("RecordNotice(pending): %v", err)
	}
	if err := s.MarkNoticeDelivered(t.Context(), first.ID); err != nil {
		t.Fatalf("MarkNoticeDelivered: %v", err)
	}
	got, err := s.UndeliveredNotices(WithSystemRole(t.Context()), 10)
	if err != nil {
		t.Fatalf("UndeliveredNotices: %v", err)
	}
	if len(got) != 1 || got[0].ID != pending.ID {
		t.Fatalf("UndeliveredNotices = %+v, want only pending notice %q", got, pending.ID)
	}
}

func TestForgeNoticeTenantIsolation(t *testing.T) {
	s := newTestStore(t)
	tenantA := seedTenant(t, s, "notice-tenant-a")
	tenantB := seedTenant(t, s, "notice-tenant-b")
	ctxA := WithTenant(t.Context(), tenantA)
	ctxB := WithTenant(t.Context(), tenantB)
	fromA, _ := seedWriteIntentAgent(t, ctxA, s, "notice-tenant-a-from")
	toA, _ := seedWriteIntentAgent(t, ctxA, s, "notice-tenant-a-to")
	fromB, _ := seedWriteIntentAgent(t, ctxB, s, "notice-tenant-b-from")
	toB, _ := seedWriteIntentAgent(t, ctxB, s, "notice-tenant-b-to")
	key := "RIG-5881:notice:tenant"
	noticeA, err := s.RecordNotice(ctxA, Notice{
		FromAccountID: fromA, ToAccountID: toA, RequestKey: key, Text: "tenant A notice",
	})
	if err != nil || noticeA.TenantID != tenantA {
		t.Fatalf("RecordNotice(A) = %+v, %v; want tenant %q", noticeA, err, tenantA)
	}
	noticeB, err := s.RecordNotice(ctxB, Notice{
		FromAccountID: fromB, ToAccountID: toB, RequestKey: key, Text: "tenant B notice",
	})
	if err != nil || noticeB.TenantID != tenantB || noticeB.ID == noticeA.ID {
		t.Fatalf("RecordNotice(B) = %+v, %v; want a distinct row in tenant %q", noticeB, err, tenantB)
	}
	gotA, err := s.UndeliveredNotices(ctxA, 10)
	if err != nil || len(gotA) != 1 || gotA[0].ID != noticeA.ID {
		t.Fatalf("UndeliveredNotices(A) = %+v, %v; want only A's row", gotA, err)
	}
	if err := s.MarkNoticeDelivered(ctxB, noticeA.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("MarkNoticeDelivered(B on A's row) = %v; want ErrNotFound", err)
	}
	gotA, err = s.UndeliveredNotices(ctxA, 10)
	if err != nil || len(gotA) != 1 || gotA[0].ID != noticeA.ID {
		t.Fatalf("UndeliveredNotices(A after B update) = %+v, %v; want A's row still pending", gotA, err)
	}
	all, err := s.UndeliveredNotices(WithSystemRole(t.Context()), 10)
	if err != nil || len(all) != 2 {
		t.Fatalf("UndeliveredNotices(system) = %+v, %v; want both tenant rows", all, err)
	}
}
