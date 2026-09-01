package sessionmanager

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type collectingCodexSwitchStore struct {
	sessions []domain.CodexAccountSwitchSession
}

func (s *collectingCodexSwitchStore) CreateCodexAccountSwitch(_ context.Context, rec domain.CodexAccountSwitch) (domain.CodexAccountSwitch, bool, error) {
	return rec, true, nil
}
func (s *collectingCodexSwitchStore) GetCodexAccountSwitch(context.Context, string) (domain.CodexAccountSwitch, bool, error) {
	return domain.CodexAccountSwitch{}, false, nil
}
func (s *collectingCodexSwitchStore) GetCodexAccountSwitchByIdempotency(context.Context, string) (domain.CodexAccountSwitch, bool, error) {
	return domain.CodexAccountSwitch{}, false, nil
}
func (s *collectingCodexSwitchStore) GetActiveCodexAccountSwitch(context.Context) (domain.CodexAccountSwitch, bool, error) {
	return domain.CodexAccountSwitch{}, false, nil
}
func (s *collectingCodexSwitchStore) UpdateCodexAccountSwitch(context.Context, domain.CodexAccountSwitch, domain.CodexAccountSwitchPhase) (bool, error) {
	return true, nil
}
func (s *collectingCodexSwitchStore) InsertCodexAccountSwitchSession(_ context.Context, _ string, rec domain.CodexAccountSwitchSession) error {
	s.sessions = append(s.sessions, rec)
	return nil
}
func (s *collectingCodexSwitchStore) ListCodexAccountSwitchSessions(context.Context, string) ([]domain.CodexAccountSwitchSession, error) {
	return append([]domain.CodexAccountSwitchSession(nil), s.sessions...), nil
}
func (s *collectingCodexSwitchStore) UpdateCodexAccountSwitchSession(context.Context, string, domain.CodexAccountSwitchSession, string, string) (bool, error) {
	return true, nil
}

func TestCodexAccountSwitchFingerprintIsVersionedAndStable(t *testing.T) {
	t.Parallel()
	first := codexAccountSwitchFingerprint("account-b", 7)
	if !strings.HasPrefix(first, "v1:") || len(first) != len("v1:")+64 {
		t.Fatalf("fingerprint = %q", first)
	}
	if got := codexAccountSwitchFingerprint("account-b", 7); got != first {
		t.Fatalf("stable fingerprint = %q, want %q", got, first)
	}
	if got := codexAccountSwitchFingerprint("account-c", 7); got == first {
		t.Fatal("target account must participate in fingerprint")
	}
	if got := codexAccountSwitchFingerprint("account-b", 8); got == first {
		t.Fatal("account revision must participate in fingerprint")
	}
}

func TestRetainCodexAccountSwitchFence(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{
		"requested", "waiting_for_safe_boundary", "stopping_sessions", "sessions_stopped",
		"checkpointing_source", "activating_target", "verifying_target", "restarting_sessions",
		"rollback_required", "recovery_required",
	} {
		if !retainCodexAccountSwitchFence(domain.CodexAccountSwitchPhase(phase)) {
			t.Fatalf("phase %s must retain the fence", phase)
		}
	}
	for _, phase := range []string{"completed", "cancelled", "failed"} {
		if retainCodexAccountSwitchFence(domain.CodexAccountSwitchPhase(phase)) {
			t.Fatalf("phase %s must release the fence", phase)
		}
	}
}

func TestCodexAccountSwitchSkipsStoppedSessionsWithoutNativeIdentity(t *testing.T) {
	store := newFakeStore()
	store.sessions["stopped-codex"] = domain.SessionRecord{
		ID: "stopped-codex", Harness: domain.HarnessCodex, Mode: domain.SessionModeTUI,
	}
	manager := New(Deps{Store: store, Runtime: &fakeRuntime{}})
	switchStore := &collectingCodexSwitchStore{}
	switchRecord := domain.CodexAccountSwitch{ID: "switch-1"}
	if err := manager.populateCodexAccountSwitchSessions(context.Background(), switchStore, &switchRecord); err != nil {
		t.Fatal(err)
	}
	if len(switchStore.sessions) != 0 || len(switchRecord.Sessions) != 0 {
		t.Fatalf("stopped session was included in switch: %#v", switchStore.sessions)
	}
}

func TestCodexAccountSwitchRejectsRunningSessionWithoutExactNativeIdentity(t *testing.T) {
	store := newFakeStore()
	store.sessions["running-codex"] = domain.SessionRecord{
		ID: "running-codex", Harness: domain.HarnessCodex, Mode: domain.SessionModeTUI,
		Metadata: domain.SessionMetadata{RuntimeHandleID: "runtime-1", RuntimeLaunchID: "generation-1"},
	}
	manager := New(Deps{Store: store, Runtime: &fakeRuntime{aliveByHandle: map[string]bool{"runtime-1": true}}})
	err := manager.populateCodexAccountSwitchSessions(context.Background(), &collectingCodexSwitchStore{}, &domain.CodexAccountSwitch{ID: "switch-1"})
	if !errors.Is(err, ports.ErrCodexRunningSessionNotResumable) {
		t.Fatalf("populate error = %v, want running-session-not-resumable", err)
	}
}

func TestCodexAccountSwitchSkipsPreservedShellAfterCodexExits(t *testing.T) {
	store := newFakeStore()
	store.sessions["exited-codex"] = domain.SessionRecord{
		ID: "exited-codex", Harness: domain.HarnessCodex, Mode: domain.SessionModeTUI,
		Metadata: domain.SessionMetadata{RuntimeHandleID: "runtime-1", RuntimeLaunchID: "generation-1"},
	}
	workloadStopped := false
	manager := New(Deps{Store: store, Runtime: &fakeRuntime{
		aliveByHandle:           map[string]bool{"runtime-1": true},
		supervisedAliveOverride: &workloadStopped,
	}})
	switchStore := &collectingCodexSwitchStore{}
	switchRecord := domain.CodexAccountSwitch{ID: "switch-1"}
	if err := manager.populateCodexAccountSwitchSessions(context.Background(), switchStore, &switchRecord); err != nil {
		t.Fatal(err)
	}
	if len(switchStore.sessions) != 0 || len(switchRecord.Sessions) != 0 {
		t.Fatalf("preserved shell was included in switch: %#v", switchStore.sessions)
	}
}

func TestCodexAccountSwitchFailsClosedWhenWorkloadProbeFails(t *testing.T) {
	store := newFakeStore()
	store.sessions["unknown-codex"] = domain.SessionRecord{
		ID: "unknown-codex", Harness: domain.HarnessCodex, Mode: domain.SessionModeTUI,
		Metadata: domain.SessionMetadata{RuntimeHandleID: "runtime-1", RuntimeLaunchID: "generation-1"},
	}
	manager := New(Deps{Store: store, Runtime: &fakeRuntime{
		aliveByHandle: map[string]bool{"runtime-1": true},
		supervisedErr: errors.New("probe unavailable"),
	}})
	err := manager.populateCodexAccountSwitchSessions(context.Background(), &collectingCodexSwitchStore{}, &domain.CodexAccountSwitch{ID: "switch-1"})
	if err == nil || !strings.Contains(err.Error(), "inspect Codex workload") {
		t.Fatalf("populate error = %v, want workload inspection failure", err)
	}
}

func TestCodexAccountSwitchRecordsRunningSessionForSameNativeResume(t *testing.T) {
	store := newFakeStore()
	store.sessions["running-codex"] = domain.SessionRecord{
		ID: "running-codex", Harness: domain.HarnessCodex, Mode: domain.SessionModeTUI,
		Metadata: domain.SessionMetadata{
			RuntimeHandleID: "runtime-1", RuntimeLaunchID: "generation-1", AgentSessionID: "native-thread-1",
		},
	}
	manager := New(Deps{Store: store, Runtime: &fakeRuntime{aliveByHandle: map[string]bool{"runtime-1": true}}})
	switchStore := &collectingCodexSwitchStore{}
	switchRecord := domain.CodexAccountSwitch{ID: "switch-1"}
	if err := manager.populateCodexAccountSwitchSessions(context.Background(), switchStore, &switchRecord); err != nil {
		t.Fatal(err)
	}
	if len(switchStore.sessions) != 1 || switchStore.sessions[0].NativeSessionID != "native-thread-1" || !switchStore.sessions[0].WasRunning {
		t.Fatalf("recorded switch sessions = %#v", switchStore.sessions)
	}
}
