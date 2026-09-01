package sessionmanager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var (
	// ErrCodexAccountSwitchInProgress means the daemon-wide mutation gate is held.
	ErrCodexAccountSwitchInProgress = ports.ErrCodexAccountSwitchInProgress
	// ErrCodexAccountAlreadyActive rejects selecting the current account.
	ErrCodexAccountAlreadyActive = ports.ErrCodexAccountAlreadyActive
	// ErrCodexActiveAccountUnavailable rejects switching without a reconciled source account.
	ErrCodexActiveAccountUnavailable = ports.ErrCodexActiveAccountUnavailable
	// ErrCodexAccountSwitchNotFound means the durable operation does not exist.
	ErrCodexAccountSwitchNotFound = ports.ErrCodexAccountSwitchNotFound
	// ErrCodexAccountSwitchCancellationUnsafe marks the durable stop boundary.
	ErrCodexAccountSwitchCancellationUnsafe = ports.ErrCodexAccountSwitchCancellationUnsafe
	// ErrCodexAccountRevisionConflict reports a stale active-account revision.
	ErrCodexAccountRevisionConflict = ports.ErrCodexAccountRevisionConflict
	// ErrCodexAccountSwitchIdempotencyConflict rejects reused mismatched keys.
	ErrCodexAccountSwitchIdempotencyConflict = ports.ErrCodexAccountSwitchIdempotencyConflict
	// ErrCodexRunningSessionNotResumable blocks switching before credential mutation.
	ErrCodexRunningSessionNotResumable = ports.ErrCodexRunningSessionNotResumable
	errCodexAccountSwitchCancelled     = errors.New("codex account switch cancelled")
)

func codexAccountSwitchFingerprint(target string, revision int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("v1\x00%s\x00%d", target, revision)))
	return "v1:" + hex.EncodeToString(sum[:])
}

func (m *Manager) codexAccountSwitchDependencies() (ports.CodexAccountCredentialManager, ports.CodexAccountSwitchStore, error) {
	credentials, ok := m.agentReadiness.(ports.CodexAccountCredentialManager)
	if !ok {
		return nil, nil, errors.New("codex account credential manager is unavailable")
	}
	store, ok := m.store.(ports.CodexAccountSwitchStore)
	if !ok {
		return nil, nil, errors.New("codex account switch store is unavailable")
	}
	return credentials, store, nil
}

func (m *Manager) acquireCodexAccountSwitchGate() bool {
	m.codexAccountSwitchMu.Lock()
	defer m.codexAccountSwitchMu.Unlock()
	if m.codexAccountSwitchActive {
		return false
	}
	m.codexAccountSwitchActive = true
	m.codexAccountSwitchWorkerRunning = true
	return true
}

// claimCodexAccountSwitchRecoveryWorker starts one recovery worker while the
// durable global mutation fence remains active. Recovery-required operations
// intentionally retain that fence between HTTP requests so no new Codex
// process can start against an ambiguous runtime credential.
func (m *Manager) claimCodexAccountSwitchRecoveryWorker() bool {
	m.codexAccountSwitchMu.Lock()
	defer m.codexAccountSwitchMu.Unlock()
	if m.codexAccountSwitchWorkerRunning {
		return false
	}
	m.codexAccountSwitchActive = true
	m.codexAccountSwitchWorkerRunning = true
	return true

}

func (m *Manager) finishCodexAccountSwitchWorker(keepFence bool) {
	m.codexAccountSwitchMu.Lock()
	m.codexAccountSwitchWorkerRunning = false
	if !keepFence {
		m.codexAccountSwitchActive = false
	}
	m.codexAccountSwitchMu.Unlock()
}

func (m *Manager) releaseIdleCodexAccountSwitchFence() {
	m.codexAccountSwitchMu.Lock()
	if !m.codexAccountSwitchWorkerRunning {
		m.codexAccountSwitchActive = false
	}
	m.codexAccountSwitchMu.Unlock()
}

func (m *Manager) codexAccountSwitchIsActive() bool {
	m.codexAccountSwitchMu.Lock()
	defer m.codexAccountSwitchMu.Unlock()
	return m.codexAccountSwitchActive
}

// CodexAccountSwitchInProgress is the daemon-wide admission fence consumed by
// controller owners outside Session Manager.
func (m *Manager) CodexAccountSwitchInProgress() bool { return m.codexAccountSwitchIsActive() }

// StartCodexAccountSwitch admits and starts one daemon-owned global account switch.
func (m *Manager) StartCodexAccountSwitch(ctx context.Context, cfg ports.CodexAccountSwitchConfig) (domain.CodexAccountSwitch, error) {
	cfg.TargetAccountID = strings.TrimSpace(cfg.TargetAccountID)
	cfg.IdempotencyKey = strings.TrimSpace(cfg.IdempotencyKey)
	if cfg.IdempotencyKey == "" {
		return domain.CodexAccountSwitch{}, errors.New("idempotency key is required")
	}
	credentials, store, err := m.codexAccountSwitchDependencies()
	if err != nil {
		return domain.CodexAccountSwitch{}, err
	}
	fingerprint := codexAccountSwitchFingerprint(cfg.TargetAccountID, cfg.ExpectedAccountRevision)
	if existing, ok, readErr := store.GetCodexAccountSwitchByIdempotency(ctx, cfg.IdempotencyKey); readErr != nil {
		return domain.CodexAccountSwitch{}, readErr
	} else if ok {
		if existing.RequestFingerprint != fingerprint {
			return existing, ErrCodexAccountSwitchIdempotencyConflict
		}
		return m.loadCodexAccountSwitchSessions(ctx, store, existing)
	}
	if _, active, readErr := store.GetActiveCodexAccountSwitch(ctx); readErr != nil {
		return domain.CodexAccountSwitch{}, readErr
	} else if active {
		return domain.CodexAccountSwitch{}, ErrCodexAccountSwitchInProgress
	}
	current := credentials.CurrentCodexActiveAccount()
	if strings.TrimSpace(current.AccountID) == "" || current.Revision < 1 {
		return domain.CodexAccountSwitch{}, ErrCodexActiveAccountUnavailable
	}
	if current.AccountID == cfg.TargetAccountID {
		return domain.CodexAccountSwitch{}, ErrCodexAccountAlreadyActive
	}
	if current.Revision != cfg.ExpectedAccountRevision {
		return domain.CodexAccountSwitch{}, ErrCodexAccountRevisionConflict
	}
	if err := credentials.VerifyCodexAccountForSwitch(ctx, cfg.TargetAccountID); err != nil {
		return domain.CodexAccountSwitch{}, err
	}
	if !m.acquireCodexAccountSwitchGate() {
		return domain.CodexAccountSwitch{}, ErrCodexAccountSwitchInProgress
	}
	releaseGate := true
	defer func() {
		if releaseGate {
			m.finishCodexAccountSwitchWorker(false)
		}
	}()
	if credentials.CodexAccountLoginInProgress() {
		return domain.CodexAccountSwitch{}, ports.ErrCodexAccountLoginInProgress
	}

	now := m.clock()
	sw := domain.CodexAccountSwitch{
		ID: uuid.NewString(), SourceAccountID: current.AccountID, TargetAccountID: cfg.TargetAccountID,
		Phase: domain.CodexAccountSwitchRequested, Reason: codexAccountSwitchReason(domain.CodexAccountSwitchRequested),
		CanCancel: true, IdempotencyKey: cfg.IdempotencyKey, RequestFingerprint: fingerprint,
		ExpectedAccountRevision: cfg.ExpectedAccountRevision, CreatedAt: now, UpdatedAt: now,
	}
	created, inserted, err := store.CreateCodexAccountSwitch(ctx, sw)
	if err != nil {
		return domain.CodexAccountSwitch{}, err
	}
	sw = created
	if !inserted {
		return m.loadCodexAccountSwitchSessions(ctx, store, sw)
	}
	if err := m.populateCodexAccountSwitchSessions(ctx, store, &sw); err != nil {
		m.failCodexAccountSwitch(ctx, store, &sw, "running_session_not_resumable")
		return sw, err
	}
	releaseGate = false
	m.agentSwitchWorkers.Add(1)
	go func() {
		defer m.agentSwitchWorkers.Done()
		m.runCodexAccountSwitch(m.backgroundContext, credentials, store, sw)
	}()
	return sw, nil
}

func (m *Manager) populateCodexAccountSwitchSessions(ctx context.Context, store ports.CodexAccountSwitchStore, sw *domain.CodexAccountSwitch) error {
	records, err := m.store.ListAllSessions(ctx)
	if err != nil {
		return err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	reviewers := m.codexReviewerLifecycle()
	for _, rec := range records {
		if rec.IsTerminated {
			continue
		}
		reviewerRunning := false
		reviewerNativeID := ""
		if reviewers != nil {
			reviewerRunning, err = reviewers.CodexReviewerRunning(ctx, rec.ID)
			if err != nil {
				return err
			}
			if reviewerRunning {
				var found bool
				reviewerNativeID, found, err = reviewers.CodexReviewerNativeSession(ctx, rec.ID)
				if err != nil {
					return err
				}
				if !found || strings.TrimSpace(reviewerNativeID) == "" {
					return fmt.Errorf("%w: %s reviewer", ErrCodexRunningSessionNotResumable, rec.ID)
				}
			}
		}
		if rec.Harness != domain.HarnessCodex && !reviewerRunning {
			continue
		}
		mode := domain.NormalizeSessionMode(rec.Mode)
		nativeID := ""
		generation := strings.TrimSpace(rec.Metadata.RuntimeLaunchID)
		wasRunning := false
		if rec.Harness == domain.HarnessCodex && mode == domain.SessionModeChat {
			nativeID = strings.TrimSpace(rec.Metadata.ProviderConversationID)
			generation = strings.TrimSpace(rec.Metadata.ControllerGeneration)
			wasRunning = m.chat != nil && m.chat.HasLiveChatController(rec.ID)
		} else if rec.Harness == domain.HarnessCodex {
			nativeID = strings.TrimSpace(rec.Metadata.AgentSessionID)
			if strings.TrimSpace(rec.Metadata.RuntimeHandleID) != "" {
				wasRunning, err = m.codexTUIWorkloadRunning(ctx, rec)
				if err != nil {
					return err
				}
			}
		}
		if rec.Harness == domain.HarnessCodex && wasRunning && nativeID == "" {
			if mode != domain.SessionModeTUI || !m.codexTUIFreshRestartSafe(ctx, rec) {
				return fmt.Errorf("%w: %s", ErrCodexRunningSessionNotResumable, rec.ID)
			}
		}
		if rec.Harness == domain.HarnessCodex && wasRunning && generation == "" {
			return fmt.Errorf("%w: %s controller generation", ErrCodexRunningSessionNotResumable, rec.ID)
		}
		if !wasRunning && !reviewerRunning {
			continue
		}
		item := domain.CodexAccountSwitchSession{
			SessionID: rec.ID, NativeSessionID: nativeID, InterfaceMode: mode,
			SourceGeneration: generation, WasRunning: wasRunning, StopState: "pending", RestartState: "pending",
			ReviewerWasRunning: reviewerRunning, ReviewerStopState: "skipped", ReviewerRestartState: "skipped",
			ReviewerNativeSessionID: reviewerNativeID,
		}
		if !wasRunning {
			item.RestartState = "skipped"
		}
		if reviewerRunning {
			item.ReviewerStopState = "pending"
			item.ReviewerRestartState = "pending"
		}
		if err := store.InsertCodexAccountSwitchSession(ctx, sw.ID, item); err != nil {
			return err
		}
		sw.Sessions = append(sw.Sessions, item)
	}
	return nil
}

// codexTUIWorkloadRunning distinguishes a live terminal host from the Codex
// process it was created to supervise. Interactive runtimes intentionally keep
// their shell alive after Codex exits so users retain scrollback; that shell is
// not a running Codex writer and must not block a global account switch.
//
// Runtime implementations without workload inspection retain the conservative
// legacy behavior: a live host is treated as a live workload. Probe failures are
// inconclusive and fail admission before any credential mutation.
func (m *Manager) codexTUIWorkloadRunning(ctx context.Context, rec domain.SessionRecord) (bool, error) {
	handle := ports.RuntimeHandle{ID: strings.TrimSpace(rec.Metadata.RuntimeHandleID)}
	hostAlive, err := m.runtime.IsAlive(ctx, handle)
	if err != nil {
		return false, fmt.Errorf("inspect Codex runtime host for %s: %w", rec.ID, err)
	}
	if !hostAlive {
		return false, nil
	}
	launchID := strings.TrimSpace(rec.Metadata.RuntimeLaunchID)
	inspector, ok := m.runtime.(ports.SupervisedProcessInspector)
	if !ok || launchID == "" {
		return true, nil
	}
	workloadAlive, err := inspector.IsSupervisedProcessAlive(ctx, handle, ports.SupervisedProcessRef{
		SessionID: rec.ID,
		LaunchID:  launchID,
	})
	if err != nil {
		return false, fmt.Errorf("inspect Codex workload for %s: %w", rec.ID, err)
	}
	return workloadAlive, nil
}

// codexTUIFreshRestartSafe positively proves that a live Codex process has not
// started a native conversation yet. The terminal-surface proof is shared with
// interface switching: missing hooks, metadata, or rollout files alone are not
// enough. An admitted switch session with an empty NativeSessionID therefore
// durably means "restart this untouched controller fresh"; older code never
// persisted that shape for a running Codex session.
func (m *Manager) codexTUIFreshRestartSafe(ctx context.Context, rec domain.SessionRecord) bool {
	if m.agents == nil {
		return false
	}
	agent, ok := m.agents.Agent(rec.Harness)
	return ok && m.nativeConversationNotStarted(ctx, rec, agent)
}

func codexAccountSwitchRestartPolicy(item domain.CodexAccountSwitchSession) (forceFresh, requireNativeHistory bool) {
	forceFresh = item.WasRunning && item.InterfaceMode == domain.SessionModeTUI && strings.TrimSpace(item.NativeSessionID) == ""
	return forceFresh, !forceFresh
}

func (m *Manager) runCodexAccountSwitch(ctx context.Context, credentials ports.CodexAccountCredentialManager, store ports.CodexAccountSwitchStore, sw domain.CodexAccountSwitch) {
	defer func() { m.finishCodexAccountSwitchWorker(retainCodexAccountSwitchFence(sw.Phase)) }()
	sessions, err := store.ListCodexAccountSwitchSessions(ctx, sw.ID)
	if err != nil {
		m.advanceCodexAccountSwitch(ctx, store, &sw, domain.CodexAccountSwitchRecoveryRequired, "switch_state_unavailable")
		return
	}
	releaseOperations, err := m.acquireCodexSwitchSessionOperations(ctx, sessions)
	if err != nil {
		m.failCodexAccountSwitch(ctx, store, &sw, "session_operation_in_progress")
		return
	}
	defer func() {
		if !retainCodexSwitchSessionOperations(sw.Phase) {
			releaseOperations()
		}
	}()
	abortChatIntake, err := m.armCodexSwitchChatIntake(ctx, sessions)
	if err != nil {
		m.failCodexAccountSwitch(ctx, store, &sw, "safe_boundary_failed")
		return
	}
	chatStopped := false
	defer func() {
		if !chatStopped {
			abortChatIntake()
		}
	}()
	if sw.Phase == domain.CodexAccountSwitchRequested {
		if !m.advanceCodexAccountSwitch(ctx, store, &sw, domain.CodexAccountSwitchWaitingSafeBoundary, "") {
			return
		}
	}
	if sw.Phase != domain.CodexAccountSwitchWaitingSafeBoundary {
		return
	}
	if err := m.waitForCodexAccountSafeBoundary(ctx, store, sw.ID, sessions); err != nil {
		if errors.Is(err, errCodexAccountSwitchCancelled) {
			return
		}
		m.failCodexAccountSwitch(ctx, store, &sw, "safe_boundary_failed")
		return
	}
	if err := m.closeCodexAccountSwitchDecisionInputs(ctx, sw.ID, sessions); err != nil {
		m.failCodexAccountSwitch(ctx, store, &sw, "safe_boundary_failed")
		return
	}
	releaseTerminalInput, err := m.freezeCodexSwitchTerminalInput(ctx, sessions)
	if err != nil {
		m.failCodexAccountSwitch(ctx, store, &sw, "safe_boundary_failed")
		return
	}
	defer releaseTerminalInput()
	if err := m.prepareCodexSwitchChatDrain(ctx, sessions); err != nil {
		m.failCodexAccountSwitch(ctx, store, &sw, "safe_boundary_failed")
		return
	}
	if current, ok, _ := store.GetCodexAccountSwitch(ctx, sw.ID); ok && current.Phase == domain.CodexAccountSwitchCancelled {
		return
	}
	if !m.advanceCodexAccountSwitch(ctx, store, &sw, domain.CodexAccountSwitchStoppingSessions, "") {
		return
	}
	if err := m.stopCodexSwitchSessions(ctx, store, sw.ID, sessions); err != nil {
		m.advanceCodexAccountSwitch(ctx, store, &sw, domain.CodexAccountSwitchRecoveryRequired, "stop_unconfirmed")
		return
	}
	chatStopped = true
	if !m.advanceCodexAccountSwitch(ctx, store, &sw, domain.CodexAccountSwitchSessionsStopped, "") ||
		!m.advanceCodexAccountSwitch(ctx, store, &sw, domain.CodexAccountSwitchCheckpointCredential, "") ||
		!m.advanceCodexAccountSwitch(ctx, store, &sw, domain.CodexAccountSwitchActivatingAccount, "") {
		return
	}
	if _, err := credentials.CheckpointAndActivateCodexAccount(ctx, sw.ID, sw.TargetAccountID, sw.ExpectedAccountRevision); err != nil {
		m.rollbackCodexAccountSwitch(ctx, credentials, store, &sw, sessions, "activation_unconfirmed")
		return
	}
	committedAt := m.clock()
	sw.CredentialsCommittedAt = &committedAt
	if !m.advanceCodexAccountSwitch(ctx, store, &sw, domain.CodexAccountSwitchVerifyingAccount, "") {
		return
	}
	if err := credentials.VerifyCurrentCodexAccount(ctx, sw.TargetAccountID); err != nil {
		m.advanceCodexAccountSwitch(ctx, store, &sw, domain.CodexAccountSwitchRecoveryRequired, "global_account_changed")
		return
	}
	if !m.advanceCodexAccountSwitch(ctx, store, &sw, domain.CodexAccountSwitchRestartingSessions, "") {
		return
	}
	if err := m.restartCodexSwitchSessions(ctx, store, sw.ID, sessions); err != nil {
		m.advanceCodexAccountSwitch(ctx, store, &sw, domain.CodexAccountSwitchRecoveryRequired, "restart_unconfirmed")
		return
	}
	completed := m.clock()
	sw.CompletedAt = &completed
	m.advanceCodexAccountSwitch(ctx, store, &sw, domain.CodexAccountSwitchCompleted, "")
}

func (m *Manager) acquireCodexSwitchSessionOperations(ctx context.Context, sessions []domain.CodexAccountSwitchSession) (func(), error) {
	acquired := make([]domain.SessionID, 0, len(sessions))
	for _, item := range sessions {
		rec, found, err := m.store.GetSession(ctx, item.SessionID)
		if err != nil {
			for i := len(acquired) - 1; i >= 0; i-- {
				m.endAgentOperation(acquired[i], agentOperationCodexAccountSwitch)
			}
			return nil, err
		}
		if !found {
			for i := len(acquired) - 1; i >= 0; i-- {
				m.endAgentOperation(acquired[i], agentOperationCodexAccountSwitch)
			}
			return nil, ErrNotFound
		}
		// A non-Codex worker may own a Codex reviewer. The review engine has its
		// own worker lock and the daemon-wide reviewer gate, so reserving the
		// worker session here would unnecessarily block unrelated Claude/other
		// harness input for the duration of the Codex credential switch.
		if rec.Harness != domain.HarnessCodex {
			continue
		}
		if err := m.beginOrReclaimCodexAccountSwitchOperation(ctx, item.SessionID); err != nil {
			for i := len(acquired) - 1; i >= 0; i-- {
				m.endAgentOperation(acquired[i], agentOperationCodexAccountSwitch)
			}
			return nil, err
		}
		acquired = append(acquired, item.SessionID)
	}
	return func() {
		for i := len(acquired) - 1; i >= 0; i-- {
			m.endAgentOperation(acquired[i], agentOperationCodexAccountSwitch)
		}
	}, nil
}

func (m *Manager) beginOrReclaimCodexAccountSwitchOperation(ctx context.Context, id domain.SessionID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.agentOpMu.Lock()
	if current, active := m.agentOperations[id]; active {
		m.agentOpMu.Unlock()
		if current == agentOperationCodexAccountSwitch {
			return nil
		}
		return errAgentOperationInProgress
	}
	m.agentOperations[id] = agentOperationCodexAccountSwitch
	drained := m.inputDrained[id]
	m.agentOpMu.Unlock()
	if drained == nil {
		return nil
	}
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		m.endAgentOperation(id, agentOperationCodexAccountSwitch)
		return ctx.Err()
	}
}

func retainCodexSwitchSessionOperations(phase domain.CodexAccountSwitchPhase) bool {
	switch phase {
	case domain.CodexAccountSwitchStoppingSessions,
		domain.CodexAccountSwitchSessionsStopped,
		domain.CodexAccountSwitchCheckpointCredential,
		domain.CodexAccountSwitchActivatingAccount,
		domain.CodexAccountSwitchVerifyingAccount,
		domain.CodexAccountSwitchRestartingSessions,
		domain.CodexAccountSwitchRollbackRequired,
		domain.CodexAccountSwitchRecoveryRequired:
		return true
	default:
		return false
	}
}

func retainCodexAccountSwitchFence(phase domain.CodexAccountSwitchPhase) bool {
	return !phase.Terminal()
}

func (m *Manager) stopCodexSwitchSessions(ctx context.Context, store ports.CodexAccountSwitchStore, switchID string, sessions []domain.CodexAccountSwitchSession) error {
	reviewers := m.codexReviewerLifecycle()
	for i := range sessions {
		item := &sessions[i]
		if item.StopState == "stopped" && item.ReviewerStopState != "pending" {
			continue
		}
		if !item.WasRunning && item.StopState != "stopped" {
			previous := item.StopState
			item.StopState = "stopped"
			m.persistCodexSwitchSession(ctx, store, switchID, *item, previous, item.RestartState)
		}
		if item.WasRunning && item.StopState != "stopped" {
			rec, ok, readErr := m.store.GetSession(ctx, item.SessionID)
			if readErr != nil || !ok {
				item.StopState, item.ErrorCode = "failed", "session_missing"
				m.persistCodexSwitchSession(ctx, store, switchID, *item, "pending", item.RestartState)
				return errors.New("session missing")
			}
			currentGeneration := strings.TrimSpace(rec.Metadata.RuntimeLaunchID)
			if item.InterfaceMode == domain.SessionModeChat {
				currentGeneration = strings.TrimSpace(rec.Metadata.ControllerGeneration)
			}
			if currentGeneration == "" || currentGeneration != item.SourceGeneration {
				item.StopState, item.ErrorCode = "failed", "source_generation_changed"
				m.persistCodexSwitchSession(ctx, store, switchID, *item, "pending", item.RestartState)
				return errors.New("codex source generation changed before shutdown")
			}
			var stopErr error
			if item.InterfaceMode == domain.SessionModeChat {
				if m.chat == nil {
					stopErr = errors.New("chat unavailable")
				} else {
					stopErr = m.chat.StopChat(ctx, item.SessionID)
				}
			} else {
				stopErr = m.stopSourceRuntime(ctx, ports.RuntimeHandle{ID: rec.Metadata.RuntimeHandleID})
			}
			if stopErr != nil {
				item.StopState, item.ErrorCode = "failed", "stop_unconfirmed"
				m.persistCodexSwitchSession(ctx, store, switchID, *item, "pending", item.RestartState)
				return stopErr
			}
			now := m.clock()
			previous := item.StopState
			item.StopState, item.StoppedAt, item.ErrorCode = "stopped", &now, ""
			m.persistCodexSwitchSession(ctx, store, switchID, *item, previous, item.RestartState)
		}
		if item.ReviewerWasRunning && item.ReviewerStopState == "pending" {
			if reviewers == nil {
				item.ReviewerStopState, item.ErrorCode = "failed", "reviewer_stop_unconfirmed"
				m.persistCodexSwitchSession(ctx, store, switchID, *item, item.StopState, item.RestartState)
				return errors.New("codex reviewer lifecycle is unavailable")
			}
			stopped, stopErr := reviewers.SuspendCodexReviewer(ctx, item.SessionID)
			if stopErr != nil || !stopped {
				item.ReviewerStopState, item.ErrorCode = "failed", "reviewer_stop_unconfirmed"
				m.persistCodexSwitchSession(ctx, store, switchID, *item, item.StopState, item.RestartState)
				if stopErr != nil {
					return stopErr
				}
				return errors.New("codex reviewer stop was not confirmed")
			}
			item.ReviewerStopState, item.ErrorCode = "stopped", ""
			m.persistCodexSwitchSession(ctx, store, switchID, *item, item.StopState, item.RestartState)
		}
	}
	return nil
}

func (m *Manager) restartCodexSwitchSessions(ctx context.Context, store ports.CodexAccountSwitchStore, switchID string, sessions []domain.CodexAccountSwitchSession) error {
	var errs []error
	reviewers := m.codexReviewerLifecycle()
	for i := range sessions {
		item := &sessions[i]
		if item.WasRunning && item.StopState == "stopped" && item.RestartState != "restarted" && item.RestartState != "skipped" {
			previousRestart := item.RestartState
			rec, ok, readErr := m.store.GetSession(ctx, item.SessionID)
			if readErr != nil || !ok {
				item.RestartState, item.ErrorCode = "failed", "restart_unconfirmed"
				m.persistCodexSwitchSession(ctx, store, switchID, *item, item.StopState, previousRestart)
				errs = append(errs, errors.New("session missing"))
				continue
			}
			project, loadErr := m.loadProject(ctx, rec.ProjectID)
			if loadErr != nil {
				item.RestartState, item.ErrorCode = "failed", "restart_unconfirmed"
				m.persistCodexSwitchSession(ctx, store, switchID, *item, item.StopState, previousRestart)
				errs = append(errs, loadErr)
				continue
			}
			ws := ports.WorkspaceInfo{Path: rec.Metadata.WorkspacePath, Branch: rec.Metadata.Branch, SessionID: rec.ID, ProjectID: rec.ProjectID}
			var handle *ports.RuntimeHandle
			if item.InterfaceMode == domain.SessionModeTUI {
				value := ports.RuntimeHandle{ID: rec.Metadata.RuntimeHandleID}
				handle = &value
			}
			forceFresh, requireNativeHistory := codexAccountSwitchRestartPolicy(*item)
			result, restartErr := m.relaunchSessionWithPolicy(
				ctx, "Codex account switch", rec, project, ws, handle, forceFresh, requireNativeHistory,
			)
			if restartErr == nil && requireNativeHistory && item.InterfaceMode == domain.SessionModeTUI && result.Mode != RestoreModeNative {
				restartErr = errors.New("codex native history resume was not selected")
			}
			if restartErr == nil && requireNativeHistory {
				resumedNativeID := strings.TrimSpace(result.Session.Metadata.AgentSessionID)
				if item.InterfaceMode == domain.SessionModeChat {
					resumedNativeID = strings.TrimSpace(result.Session.Metadata.ProviderConversationID)
				}
				if resumedNativeID != item.NativeSessionID {
					restartErr = errors.New("codex resumed a different native history")
				}
			}
			if restartErr != nil {
				item.RestartState, item.ErrorCode = "failed", "restart_unconfirmed"
				errs = append(errs, restartErr)
			} else {
				at := m.clock()
				item.RestartState, item.RestartedAt, item.ErrorCode = "restarted", &at, ""
				item.TargetGeneration = result.Session.Metadata.RuntimeLaunchID
				if item.InterfaceMode == domain.SessionModeChat {
					item.TargetGeneration = result.Session.Metadata.ControllerGeneration
				}
			}
			m.persistCodexSwitchSession(ctx, store, switchID, *item, item.StopState, previousRestart)
		}
		if item.ReviewerWasRunning && item.ReviewerStopState == "stopped" && item.ReviewerRestartState != "restarted" && item.ReviewerRestartState != "skipped" {
			if reviewers == nil {
				item.ReviewerRestartState, item.ErrorCode = "failed", "reviewer_restart_unconfirmed"
				errs = append(errs, errors.New("codex reviewer lifecycle is unavailable"))
			} else if err := reviewers.RestoreCodexReviewer(ctx, item.SessionID); err != nil {
				item.ReviewerRestartState, item.ErrorCode = "failed", "reviewer_restart_unconfirmed"
				errs = append(errs, err)
			} else {
				nativeID, found, nativeErr := reviewers.CodexReviewerNativeSession(ctx, item.SessionID)
				if nativeErr != nil || !found || nativeID != item.ReviewerNativeSessionID {
					item.ReviewerRestartState, item.ErrorCode = "failed", "reviewer_native_history_changed"
					if nativeErr == nil {
						nativeErr = errors.New("codex reviewer did not resume the recorded native history")
					}
					errs = append(errs, nativeErr)
				} else {
					item.ReviewerRestartState, item.ErrorCode = "restarted", ""
				}
			}
			m.persistCodexSwitchSession(ctx, store, switchID, *item, item.StopState, item.RestartState)
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) rollbackCodexAccountSwitch(ctx context.Context, credentials ports.CodexAccountCredentialManager, store ports.CodexAccountSwitchStore, sw *domain.CodexAccountSwitch, sessions []domain.CodexAccountSwitchSession, code string) {
	if !m.advanceCodexAccountSwitch(ctx, store, sw, domain.CodexAccountSwitchRollbackRequired, code) {
		return
	}
	if err := credentials.RestoreCodexAccountCredential(ctx, sw.SourceAccountID, sw.TargetAccountID); err != nil {
		m.advanceCodexAccountSwitch(ctx, store, sw, domain.CodexAccountSwitchRecoveryRequired, "rollback_unconfirmed")
		return
	}
	if err := m.restartCodexSwitchSessions(ctx, store, sw.ID, sessions); err != nil {
		m.advanceCodexAccountSwitch(ctx, store, sw, domain.CodexAccountSwitchRecoveryRequired, "restart_unconfirmed")
		return
	}
	completed := m.clock()
	sw.CompletedAt = &completed
	m.advanceCodexAccountSwitch(ctx, store, sw, domain.CodexAccountSwitchFailed, code)
}

func (m *Manager) waitForCodexAccountSafeBoundary(ctx context.Context, store ports.CodexAccountSwitchStore, switchID string, sessions []domain.CodexAccountSwitchSession) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	reviewers := m.codexReviewerLifecycle()
	for {
		if current, ok, err := store.GetCodexAccountSwitch(ctx, switchID); err != nil {
			return err
		} else if ok && current.Phase == domain.CodexAccountSwitchCancelled {
			return errCodexAccountSwitchCancelled
		}
		allSafe := true
		for _, item := range sessions {
			if item.WasRunning {
				rec, ok, err := m.store.GetSession(ctx, item.SessionID)
				if err != nil || !ok {
					return errors.New("session unavailable")
				}
				if item.InterfaceMode == domain.SessionModeTUI && (rec.Activity.State == domain.ActivityWaitingInput || rec.Activity.State == domain.ActivityBlocked) {
					m.allowCodexAccountSwitchDecisionInput(item.SessionID, switchID)
				} else if err := m.closeCodexAccountSwitchDecisionInput(ctx, item.SessionID, switchID); err != nil {
					return err
				}
				if rec.Activity.State == domain.ActivityActive || rec.Activity.State == domain.ActivityWaitingInput || rec.Activity.State == domain.ActivityBlocked {
					allSafe = false
					break
				}
			}
			if item.ReviewerWasRunning {
				if reviewers == nil {
					return errors.New("codex reviewer lifecycle is unavailable")
				}
				busy, err := reviewers.CodexReviewerBusy(ctx, item.SessionID)
				if err != nil {
					return err
				}
				if busy {
					allSafe = false
					break
				}
			}
		}
		if allSafe {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m *Manager) allowCodexAccountSwitchDecisionInput(id domain.SessionID, switchID string) {
	m.agentOpMu.Lock()
	defer m.agentOpMu.Unlock()
	if m.agentOperations[id] != agentOperationCodexAccountSwitch {
		return
	}
	if m.codexAccountSwitchDecisionInput == nil {
		m.codexAccountSwitchDecisionInput = make(map[domain.SessionID]string)
	}
	m.codexAccountSwitchDecisionInput[id] = switchID
}

func (m *Manager) closeCodexAccountSwitchDecisionInput(ctx context.Context, id domain.SessionID, switchID string) error {
	m.agentOpMu.Lock()
	if current, ok := m.codexAccountSwitchDecisionInput[id]; !ok || current != switchID {
		m.agentOpMu.Unlock()
		return nil
	}
	delete(m.codexAccountSwitchDecisionInput, id)
	drained := m.inputDrained[id]
	m.agentOpMu.Unlock()
	if drained == nil {
		return nil
	}
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) closeCodexAccountSwitchDecisionInputs(ctx context.Context, switchID string, sessions []domain.CodexAccountSwitchSession) error {
	for _, item := range sessions {
		if err := m.closeCodexAccountSwitchDecisionInput(ctx, item.SessionID, switchID); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) armCodexSwitchChatIntake(ctx context.Context, sessions []domain.CodexAccountSwitchSession) (func(), error) {
	handoff, ok := m.chat.(chatHandoffLauncher)
	if !ok {
		for _, item := range sessions {
			if item.WasRunning && item.InterfaceMode == domain.SessionModeChat {
				return func() {}, errors.New("codex Chat drain is unavailable")
			}
		}
		return func() {}, nil
	}
	armed := make([]domain.SessionID, 0)
	for _, item := range sessions {
		if !item.WasRunning || item.InterfaceMode != domain.SessionModeChat {
			continue
		}
		if err := handoff.ArmChatHandoff(ctx, item.SessionID, domain.SessionInterfaceTransitionDrain); err != nil {
			for _, id := range armed {
				handoff.AbortChatHandoff(id)
			}
			return func() {}, err
		}
		armed = append(armed, item.SessionID)
	}
	return func() {
		for _, id := range armed {
			handoff.AbortChatHandoff(id)
		}
	}, nil
}

func (m *Manager) prepareCodexSwitchChatDrain(ctx context.Context, sessions []domain.CodexAccountSwitchSession) error {
	handoff, ok := m.chat.(chatHandoffLauncher)
	if !ok {
		return nil
	}
	for _, item := range sessions {
		if item.WasRunning && item.InterfaceMode == domain.SessionModeChat {
			if err := handoff.PrepareChatHandoff(ctx, item.SessionID, domain.SessionInterfaceTransitionDrain); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) freezeCodexSwitchTerminalInput(ctx context.Context, sessions []domain.CodexAccountSwitchSession) (func(), error) {
	releases := make([]func(), 0)
	releaseAll := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for _, item := range sessions {
		if !item.WasRunning || item.InterfaceMode != domain.SessionModeTUI {
			continue
		}
		rec, ok, err := m.store.GetSession(ctx, item.SessionID)
		if err != nil || !ok {
			releaseAll()
			if err != nil {
				return func() {}, err
			}
			return func() {}, ErrNotFound
		}
		lastInputAt, release := m.beginTerminalInputDrain(rec)
		if release != nil {
			releases = append(releases, release)
		}
		if rec.Activity.State == domain.ActivityActive || rec.Activity.State == domain.ActivityBlocked || lastInputAt.After(rec.Activity.LastActivityAt) {
			releaseAll()
			return func() {}, errors.New("codex terminal activity changed at the switch boundary")
		}
	}
	return releaseAll, nil
}

func (m *Manager) persistCodexSwitchSession(ctx context.Context, store ports.CodexAccountSwitchStore, switchID string, item domain.CodexAccountSwitchSession, expectedStop, expectedRestart string) {
	_, _ = store.UpdateCodexAccountSwitchSession(ctx, switchID, item, expectedStop, expectedRestart)
}

func (m *Manager) advanceCodexAccountSwitch(ctx context.Context, store ports.CodexAccountSwitchStore, sw *domain.CodexAccountSwitch, next domain.CodexAccountSwitchPhase, code string) bool {
	expected := sw.Phase
	candidate := *sw
	candidate.Phase, candidate.FailureCode, candidate.Reason, candidate.UpdatedAt = next, code, codexAccountSwitchReason(next), m.clock()
	candidate.CanCancel = next == domain.CodexAccountSwitchRequested || next == domain.CodexAccountSwitchWaitingSafeBoundary
	candidate.CanRecover = next == domain.CodexAccountSwitchRecoveryRequired
	ok, err := store.UpdateCodexAccountSwitch(ctx, candidate, expected)
	if err == nil && ok {
		*sw = candidate
		m.publishCodexAccountSwitchChanged()
	} else if err == nil {
		// A concurrent cancellation or recovery owns the durable result. Refresh
		// the local phase so deferred fence cleanup follows durable state instead
		// of the transition this worker merely attempted.
		if current, found, readErr := store.GetCodexAccountSwitch(ctx, sw.ID); readErr == nil && found {
			*sw = current
			sw.Reason = codexAccountSwitchReason(current.Phase)
			sw.CanCancel = current.Phase == domain.CodexAccountSwitchRequested || current.Phase == domain.CodexAccountSwitchWaitingSafeBoundary
			sw.CanRecover = current.Phase == domain.CodexAccountSwitchRecoveryRequired
		}
	}
	return err == nil && ok
}

func (m *Manager) failCodexAccountSwitch(ctx context.Context, store ports.CodexAccountSwitchStore, sw *domain.CodexAccountSwitch, code string) {
	completed := m.clock()
	sw.CompletedAt = &completed
	m.advanceCodexAccountSwitch(ctx, store, sw, domain.CodexAccountSwitchFailed, code)
}

func codexAccountSwitchReason(phase domain.CodexAccountSwitchPhase) string {
	switch phase {
	case domain.CodexAccountSwitchWaitingSafeBoundary:
		return "Waiting for Codex sessions to reach a safe point."
	case domain.CodexAccountSwitchStoppingSessions:
		return "Stopping AO Codex sessions."
	case domain.CodexAccountSwitchSessionsStopped, domain.CodexAccountSwitchCheckpointCredential:
		return "Saving the active Codex account."
	case domain.CodexAccountSwitchActivatingAccount, domain.CodexAccountSwitchVerifyingAccount:
		return "Activating the selected Codex account."
	case domain.CodexAccountSwitchRestartingSessions:
		return "Restarting AO Codex sessions from their existing history."
	case domain.CodexAccountSwitchRollbackRequired:
		return "Restoring the previous Codex account."
	case domain.CodexAccountSwitchCompleted:
		return "Codex account switch completed."
	case domain.CodexAccountSwitchRecoveryRequired:
		return "Some Codex sessions need recovery."
	case domain.CodexAccountSwitchCancelled:
		return "Codex account switch cancelled."
	case domain.CodexAccountSwitchFailed:
		return "Codex account switch failed."
	default:
		return "Preparing the Codex account switch."
	}
}

func (m *Manager) loadCodexAccountSwitchSessions(ctx context.Context, store ports.CodexAccountSwitchStore, sw domain.CodexAccountSwitch) (domain.CodexAccountSwitch, error) {
	sessions, err := store.ListCodexAccountSwitchSessions(ctx, sw.ID)
	if err != nil {
		return sw, err
	}
	sw.Sessions = sessions
	sw.Reason = codexAccountSwitchReason(sw.Phase)
	sw.CanCancel = sw.Phase == domain.CodexAccountSwitchRequested || sw.Phase == domain.CodexAccountSwitchWaitingSafeBoundary
	sw.CanRecover = sw.Phase == domain.CodexAccountSwitchRecoveryRequired
	return sw, nil
}

// GetCodexAccountSwitch returns one durable switch with its affected sessions.
func (m *Manager) GetCodexAccountSwitch(ctx context.Context, id string) (domain.CodexAccountSwitch, error) {
	_, store, err := m.codexAccountSwitchDependencies()
	if err != nil {
		return domain.CodexAccountSwitch{}, err
	}
	sw, ok, err := store.GetCodexAccountSwitch(ctx, strings.TrimSpace(id))
	if err != nil {
		return sw, err
	}
	if !ok {
		return sw, ErrCodexAccountSwitchNotFound
	}
	return m.loadCodexAccountSwitchSessions(ctx, store, sw)
}

// GetActiveCodexAccountSwitch returns the sole nonterminal switch when present.
func (m *Manager) GetActiveCodexAccountSwitch(ctx context.Context) (domain.CodexAccountSwitch, bool, error) {
	_, store, err := m.codexAccountSwitchDependencies()
	if err != nil {
		return domain.CodexAccountSwitch{}, false, err
	}
	sw, ok, err := store.GetActiveCodexAccountSwitch(ctx)
	if err != nil || !ok {
		return sw, ok, err
	}
	sw, err = m.loadCodexAccountSwitchSessions(ctx, store, sw)
	return sw, err == nil, err
}

// CancelCodexAccountSwitch cancels a switch before stopping sessions becomes durable.
func (m *Manager) CancelCodexAccountSwitch(ctx context.Context, id string) (domain.CodexAccountSwitch, error) {
	_, store, err := m.codexAccountSwitchDependencies()
	if err != nil {
		return domain.CodexAccountSwitch{}, err
	}
	sw, err := m.GetCodexAccountSwitch(ctx, id)
	if err != nil {
		return sw, err
	}
	if !sw.CanCancel {
		return sw, ErrCodexAccountSwitchCancellationUnsafe
	}
	now := m.clock()
	sw.CancellationRequestedAt, sw.CompletedAt = &now, &now
	if !m.advanceCodexAccountSwitch(ctx, store, &sw, domain.CodexAccountSwitchCancelled, "") {
		return sw, errors.New("codex account switch changed concurrently")
	}
	m.releaseIdleCodexAccountSwitchFence()
	return sw, nil
}

// RecoverCodexAccountSwitch retries the exact incomplete durable operation.
func (m *Manager) RecoverCodexAccountSwitch(ctx context.Context, id string) (domain.CodexAccountSwitch, error) {
	credentials, store, err := m.codexAccountSwitchDependencies()
	if err != nil {
		return domain.CodexAccountSwitch{}, err
	}
	sw, err := m.GetCodexAccountSwitch(ctx, id)
	if err != nil {
		return sw, err
	}
	if sw.Phase != domain.CodexAccountSwitchRecoveryRequired {
		return sw, errors.New("codex account switch does not require recovery")
	}
	if !m.claimCodexAccountSwitchRecoveryWorker() {
		return sw, ErrCodexAccountSwitchInProgress
	}
	m.agentSwitchWorkers.Add(1)
	go func() {
		defer m.agentSwitchWorkers.Done()
		m.recoverCodexAccountSwitch(m.backgroundContext, credentials, store, sw)
	}()
	return sw, nil
}

func (m *Manager) recoverCodexAccountSwitch(ctx context.Context, credentials ports.CodexAccountCredentialManager, store ports.CodexAccountSwitchStore, sw domain.CodexAccountSwitch) {
	defer func() { m.finishCodexAccountSwitchWorker(retainCodexAccountSwitchFence(sw.Phase)) }()
	sessions, err := store.ListCodexAccountSwitchSessions(ctx, sw.ID)
	if err != nil {
		return
	}
	releaseOperations, err := m.acquireCodexSwitchSessionOperations(ctx, sessions)
	if err != nil {
		return
	}
	defer func() {
		if !retainCodexSwitchSessionOperations(sw.Phase) {
			releaseOperations()
		}
	}()
	active := credentials.CurrentCodexActiveAccount()
	targetCommitted := sw.CredentialsCommittedAt != nil || active.AccountID == sw.TargetAccountID
	if targetCommitted {
		if err := credentials.VerifyCurrentCodexAccount(ctx, sw.TargetAccountID); err != nil {
			return
		}
		if !m.advanceCodexAccountSwitch(ctx, store, &sw, domain.CodexAccountSwitchRestartingSessions, "") {
			return
		}
		if err := m.restartCodexSwitchSessions(ctx, store, sw.ID, sessions); err != nil {
			m.advanceCodexAccountSwitch(ctx, store, &sw, domain.CodexAccountSwitchRecoveryRequired, "restart_unconfirmed")
			return
		}
		completed := m.clock()
		sw.CompletedAt = &completed
		m.advanceCodexAccountSwitch(ctx, store, &sw, domain.CodexAccountSwitchCompleted, "")
		return
	}
	if err := credentials.RestoreCodexAccountCredential(ctx, sw.SourceAccountID, sw.TargetAccountID); err != nil {
		return
	}
	if !m.advanceCodexAccountSwitch(ctx, store, &sw, domain.CodexAccountSwitchRestartingSessions, "") {
		return
	}
	if err := m.restartCodexSwitchSessions(ctx, store, sw.ID, sessions); err != nil {
		m.advanceCodexAccountSwitch(ctx, store, &sw, domain.CodexAccountSwitchRecoveryRequired, "restart_unconfirmed")
		return
	}
	completed := m.clock()
	sw.CompletedAt = &completed
	m.advanceCodexAccountSwitch(ctx, store, &sw, domain.CodexAccountSwitchFailed, "activation_unconfirmed")
}

// ReconcileCodexAccountSwitches restores the daemon-wide mutation fence before
// ordinary session adoption, then resumes the exact durable operation on the
// daemon context. The safety pass does not wait for active turns to finish.
func (m *Manager) ReconcileCodexAccountSwitches(ctx context.Context) error {
	credentials, store, err := m.codexAccountSwitchDependencies()
	if err != nil {
		return nil //nolint:nilerr // account switching is optional when its feature wiring is absent.
	}
	sw, ok, err := store.GetActiveCodexAccountSwitch(ctx)
	if err != nil || !ok {
		return err
	}
	if err := credentials.WaitCodexAccountBootstrap(ctx); err != nil {
		return err
	}
	sw, err = m.loadCodexAccountSwitchSessions(ctx, store, sw)
	if err != nil {
		return err
	}
	if !m.acquireCodexAccountSwitchGate() {
		return ErrCodexAccountSwitchInProgress
	}
	if sw.Phase == domain.CodexAccountSwitchRequested || sw.Phase == domain.CodexAccountSwitchWaitingSafeBoundary {
		m.agentSwitchWorkers.Add(1)
		go func() {
			defer m.agentSwitchWorkers.Done()
			select {
			case <-m.startupBackgroundReconcileDone:
			case <-m.backgroundContext.Done():
				m.finishCodexAccountSwitchWorker(false)
				return
			}
			m.runCodexAccountSwitch(m.backgroundContext, credentials, store, sw)
		}()
		return nil
	}
	if sw.Phase != domain.CodexAccountSwitchRecoveryRequired {
		if !m.advanceCodexAccountSwitch(ctx, store, &sw, domain.CodexAccountSwitchRecoveryRequired, "daemon_restart_recovery") {
			m.finishCodexAccountSwitchWorker(false)
			return errors.New("codex account switch reconciliation changed concurrently")
		}
	}
	m.agentSwitchWorkers.Add(1)
	go func() {
		defer m.agentSwitchWorkers.Done()
		select {
		case <-m.startupBackgroundReconcileDone:
		case <-m.backgroundContext.Done():
			m.finishCodexAccountSwitchWorker(false)
			return
		}
		m.recoverCodexAccountSwitch(m.backgroundContext, credentials, store, sw)
	}()
	return nil
}
