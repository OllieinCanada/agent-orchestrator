package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestCodexActiveAccountUsesRevisionCAS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)

	if _, ok, err := st.GetCodexActiveAccount(ctx); err != nil || ok {
		t.Fatalf("initial active account: ok=%v err=%v", ok, err)
	}
	first, err := st.SetCodexActiveAccount(ctx, "account-a", 0, now)
	if err != nil || first.AccountID != "account-a" || first.Revision != 1 {
		t.Fatalf("create active account: got=%+v err=%v", first, err)
	}
	if _, err := st.SetCodexActiveAccount(ctx, "account-b", 0, now); !errors.Is(err, ports.ErrCodexAccountRevisionConflict) {
		t.Fatalf("duplicate initial revision error = %v", err)
	}
	second, err := st.SetCodexActiveAccount(ctx, "account-b", 1, now.Add(time.Second))
	if err != nil || second.AccountID != "account-b" || second.Revision != 2 {
		t.Fatalf("advance active account: got=%+v err=%v", second, err)
	}
	if _, err := st.SetCodexActiveAccount(ctx, "account-c", 1, now.Add(2*time.Second)); !errors.Is(err, ports.ErrCodexAccountRevisionConflict) {
		t.Fatalf("stale revision error = %v", err)
	}
	signedOut, err := st.SetCodexActiveAccount(ctx, "", 2, now.Add(3*time.Second))
	if err != nil || signedOut.AccountID != "" || signedOut.Revision != 3 {
		t.Fatalf("clear active account: got=%+v err=%v", signedOut, err)
	}
}

func TestCodexAccountSwitchIdempotencyAndSingleActiveConstraint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	first := domain.CodexAccountSwitch{
		ID: "switch-a", SourceAccountID: "account-a", TargetAccountID: "account-b",
		IdempotencyKey: "request-a", RequestFingerprint: "v1:first", ExpectedAccountRevision: 1,
		Phase: domain.CodexAccountSwitchRequested, CreatedAt: now, UpdatedAt: now,
	}

	created, inserted, err := st.CreateCodexAccountSwitch(ctx, first)
	if err != nil || !inserted || created.ID != first.ID {
		t.Fatalf("create switch: got=%+v inserted=%v err=%v", created, inserted, err)
	}
	replayed, inserted, err := st.CreateCodexAccountSwitch(ctx, first)
	if err != nil || inserted || replayed.ID != first.ID {
		t.Fatalf("replay switch: got=%+v inserted=%v err=%v", replayed, inserted, err)
	}
	conflict := first
	conflict.ID = "switch-b"
	conflict.RequestFingerprint = "v1:different"
	if _, _, err := st.CreateCodexAccountSwitch(ctx, conflict); !errors.Is(err, ports.ErrCodexAccountSwitchIdempotencyConflict) {
		t.Fatalf("idempotency conflict error = %v", err)
	}
	other := first
	other.ID = "switch-c"
	other.IdempotencyKey = "request-c"
	other.RequestFingerprint = "v1:other"
	if _, _, err := st.CreateCodexAccountSwitch(ctx, other); !errors.Is(err, ports.ErrCodexAccountSwitchInProgress) {
		t.Fatalf("active switch conflict error = %v", err)
	}
}

func TestCodexAccountSwitchAndSessionTransitionsAreCompareAndSwap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newTestStore(t)
	seedProject(t, st, "codex-switch")
	rec := sampleRecord("codex-switch")
	rec.Harness = domain.HarnessCodex
	session, err := st.CreateSession(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	sw := domain.CodexAccountSwitch{
		ID: "switch-cas", SourceAccountID: "account-a", TargetAccountID: "account-b",
		IdempotencyKey: "request-cas", RequestFingerprint: "v1:cas", ExpectedAccountRevision: 1,
		Phase: domain.CodexAccountSwitchRequested, CreatedAt: now, UpdatedAt: now,
	}
	if _, _, err := st.CreateCodexAccountSwitch(ctx, sw); err != nil {
		t.Fatal(err)
	}
	switchSession := domain.CodexAccountSwitchSession{
		SessionID: session.ID, NativeSessionID: "native-a", InterfaceMode: domain.SessionModeTUI,
		WasRunning: true, StopState: "pending", RestartState: "pending",
		ReviewerStopState: "skipped", ReviewerRestartState: "skipped",
	}
	if err := st.InsertCodexAccountSwitchSession(ctx, sw.ID, switchSession); err != nil {
		t.Fatal(err)
	}
	switchSession.StopState = "stopped"
	stopped := now.Add(time.Second)
	switchSession.StoppedAt = &stopped
	if ok, err := st.UpdateCodexAccountSwitchSession(ctx, sw.ID, switchSession, "pending", "pending"); err != nil || !ok {
		t.Fatalf("session transition: ok=%v err=%v", ok, err)
	}
	if ok, err := st.UpdateCodexAccountSwitchSession(ctx, sw.ID, switchSession, "pending", "pending"); err != nil || ok {
		t.Fatalf("stale session transition: ok=%v err=%v", ok, err)
	}
	sw.Phase = domain.CodexAccountSwitchWaitingSafeBoundary
	sw.UpdatedAt = stopped
	if ok, err := st.UpdateCodexAccountSwitch(ctx, sw, domain.CodexAccountSwitchRequested); err != nil || !ok {
		t.Fatalf("switch transition: ok=%v err=%v", ok, err)
	}
	if ok, err := st.UpdateCodexAccountSwitch(ctx, sw, domain.CodexAccountSwitchRequested); err != nil || ok {
		t.Fatalf("stale switch transition: ok=%v err=%v", ok, err)
	}
}
