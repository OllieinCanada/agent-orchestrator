package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/shellterm"
)

const (
	codexAccountDisplayTTL    = 5 * time.Minute
	codexAccountLaunchTTL     = 30 * time.Second
	codexAccountAuthTimeout   = 10 * time.Second
	codexAccountUsageTTL      = 5 * time.Minute
	codexAccountLoginLifetime = 15 * time.Minute
	codexAccountProcessLimit  = 2
)

// CodexAccounts is the display-safe account-management view. Credentials and
// filesystem locations remain daemon-private.
type CodexAccounts struct {
	ActiveAccountID        string                              `json:"activeAccountId,omitempty"`
	AccountRevision        int64                               `json:"accountRevision"`
	Accounts               []domain.CodexAccountSnapshot       `json:"accounts"`
	Capabilities           domain.CodexAccountCapabilities     `json:"capabilities"`
	UnmanagedGlobalAccount *domain.CodexUnmanagedGlobalAccount `json:"unmanagedGlobalAccount,omitempty"`
	CurrentSwitch          *domain.CodexAccountSwitch          `json:"currentSwitch,omitempty"`
}

// CodexAccountLoginTerminalStart combines safe login state with its trusted terminal.
type CodexAccountLoginTerminalStart struct {
	Operation     domain.CodexAccountLoginOperation `json:"operation"`
	ShellTerminal shellterm.ShellTerminal           `json:"shellTerminal"`
}

type codexAccountLoginTerminalService interface {
	OpenCommandTerminal(context.Context, shellterm.OpenCommandTerminalInput) (shellterm.ShellTerminal, error)
	CloseShellTerminal(context.Context, string) error
}

// CodexAccountStateStore persists only the active-account pointer and revision.
// Account descriptors and credentials remain filesystem-owned.
type CodexAccountStateStore interface {
	GetCodexActiveAccount(context.Context) (domain.CodexActiveAccount, bool, error)
	SetCodexActiveAccount(context.Context, string, int64, time.Time) (domain.CodexActiveAccount, error)
}

type accountAuthCall struct{ done chan struct{} }
type accountAuthState struct {
	invalidated bool
	failures    int
	nextRetryAt time.Time
	call        *accountAuthCall
}
type accountUsageState struct {
	value     *domain.CodexAccountUsageSummary
	checkedAt time.Time
	call      chan struct{}
}
type accountReconcileCall struct {
	done chan struct{}
	err  error
}
type accountLoginOperation struct {
	snapshot       domain.CodexAccountLoginOperation
	pendingDir     string
	home           string
	terminalHandle string
	closing        bool
	committing     bool
	commitDone     chan struct{}
}

type codexAccountManager struct {
	ctx               context.Context
	catalog           *codexAccountCatalog
	factory           ports.CodexAccountClientFactory
	stateStore        CodexAccountStateStore
	logger            *slog.Logger
	now               func() time.Time
	newID             func() string
	processes         chan struct{}
	executable        func() (string, error)
	terminal          codexAccountLoginTerminalService
	globalHome        string
	pendingRoot       string
	switchStagingRoot string

	mu            sync.Mutex
	bootstrapOnce sync.Once
	bootstrapDone chan struct{}
	bootstrapErr  error
	auth          map[string]*accountAuthState
	usage         map[string]*accountUsageState
	capabilities  domain.CodexAccountCapabilities
	active        domain.CodexActiveAccount
	globalAuth    domain.AgentAuthenticationObservation
	unmanaged     *domain.CodexUnmanagedGlobalAccount
	login         *accountLoginOperation
	reconcile     *accountReconcileCall
	bootstrapped  bool
	capacity      *codexCapacityCoordinator
	subscribers   map[chan CodexAccounts]struct{}
}

func newCodexAccountManager(ctx context.Context, accountRoot, pendingRoot, switchStagingRoot, globalHome string, factory ports.CodexAccountClientFactory, stateStore CodexAccountStateStore, logger *slog.Logger) *codexAccountManager {
	if ctx == nil {
		ctx = context.Background()
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	m := &codexAccountManager{
		ctx: ctx, catalog: newCodexAccountCatalog(accountRoot, logger), factory: factory, stateStore: stateStore,
		logger: logger, now: func() time.Time { return time.Now().UTC() }, newID: uuid.NewString,
		processes: make(chan struct{}, codexAccountProcessLimit), executable: os.Executable,
		globalHome: canonicalPath(globalHome), pendingRoot: canonicalPath(pendingRoot), switchStagingRoot: canonicalPath(switchStagingRoot),
		auth: map[string]*accountAuthState{}, usage: map[string]*accountUsageState{},
		capabilities: unavailableCodexCapabilities(), subscribers: map[chan CodexAccounts]struct{}{}, bootstrapDone: make(chan struct{}),
		globalAuth: uncheckedAuthentication(),
	}
	m.capacity = newCodexCapacityCoordinator(m)
	m.catalog.setOnRemoved(func(ids []string) { m.capacity.removeAccounts(ids); m.publish() })
	return m
}

func unavailableCodexCapabilities() domain.CodexAccountCapabilities {
	unknown := domain.CodexCapabilityObservation{State: domain.CodexCapabilityUnknown, ReasonCode: domain.CodexCapabilityReasonUnknown, Reason: "Codex capability detection has not completed."}
	return domain.CodexAccountCapabilities{AccountRead: unknown, NativeLogin: unknown, CapacityRead: unknown, UsageRead: unknown, ThreadResume: unknown, AccountManagement: unknown, GlobalSwitch: unknown}
}

func (m *codexAccountManager) detectCapabilities(ctx context.Context) domain.CodexAccountCapabilities {
	if m.factory == nil {
		return unavailableCodexCapabilities()
	}
	capabilities := m.factory.Capabilities(ctx)
	if capabilities.AccountRead.State == domain.CodexCapabilitySupported {
		if err := m.validateGlobalCredentialStore(); err != nil {
			capabilities.GlobalSwitch = domain.CodexCapabilityObservation{
				State: domain.CodexCapabilityUnsupported, ReasonCode: "global_credential_store_unsupported",
				Reason: "Device-global account switching requires a file-backed Codex sign-in.",
			}
		}
	}
	m.mu.Lock()
	m.capabilities = capabilities
	m.mu.Unlock()
	return capabilities
}

func (m *codexAccountManager) view(ids []string) (CodexAccounts, error) {
	records, err := m.catalog.recordsFor(ids)
	if err != nil {
		return CodexAccounts{}, mapUnknownCodexAccount(err)
	}
	m.mu.Lock()
	active, capabilities, unmanaged := m.active, m.capabilities, m.unmanaged
	m.mu.Unlock()
	accounts := make([]domain.CodexAccountSnapshot, 0, len(records))
	for _, record := range records {
		snapshot := record.Snapshot
		snapshot.Active = snapshot.ID == active.AccountID
		snapshot.Capacity = m.capacity.snapshot(snapshot.ID)
		m.mu.Lock()
		if usage := m.usage[snapshot.ID]; usage != nil && usage.value != nil {
			usageCopy := *usage.value
			snapshot.UsageSummary = &usageCopy
		}
		m.mu.Unlock()
		accounts = append(accounts, snapshot)
	}
	if active.AccountID != "" {
		for i := range accounts {
			if accounts[i].ID == active.AccountID && i > 0 {
				item := accounts[i]
				copy(accounts[1:i+1], accounts[0:i])
				accounts[0] = item
				break
			}
		}
	}
	return CodexAccounts{ActiveAccountID: active.AccountID, AccountRevision: active.Revision, Accounts: accounts, Capabilities: capabilities, UnmanagedGlobalAccount: unmanaged}, nil
}

func (m *codexAccountManager) cached() CodexAccounts { result, _ := m.view(nil); return result }

func (m *codexAccountManager) accountContext(record codexAccountRecord) ports.CodexAccountContext {
	home := record.Home
	m.mu.Lock()
	active := m.active.AccountID
	m.mu.Unlock()
	if record.Snapshot.ID == active {
		return ports.CodexAccountContext{Home: m.globalHome, Managed: false}
	}
	return ports.CodexAccountContext{Home: home, Managed: true}
}

func (m *codexAccountManager) ensure(ctx context.Context, ids []string, includeUsage bool, installation domain.AgentInstallationState) (CodexAccounts, error) {
	if err := m.catalog.refresh(); err != nil {
		return CodexAccounts{}, apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex account discovery is unavailable")
	}
	records, err := m.catalog.recordsFor(ids)
	if err != nil {
		return CodexAccounts{}, mapUnknownCodexAccount(err)
	}
	if installation == domain.AgentInstallationNotInstalled {
		return m.view(ids)
	}
	if err := m.reconcileGlobal(ctx); err != nil && !errors.Is(err, context.Canceled) {
		m.logger.Debug("Codex global account reconciliation degraded", "failure_category", "global_account_reconciliation")
	}
	capabilities := m.detectCapabilities(ctx)
	for _, record := range records {
		if record.Snapshot.Status == domain.CodexAccountStatusValid && capabilities.AccountRead.State == domain.CodexCapabilitySupported {
			if _, err := m.ensureAuthentication(ctx, record, domain.AgentReadinessPurposeDisplay, false); err != nil {
				return CodexAccounts{}, err
			}
		}
	}
	records, err = m.catalog.recordsFor(ids)
	if err != nil {
		return CodexAccounts{}, mapUnknownCodexAccount(err)
	}
	if err := m.capacity.ensure(ctx, records, capabilities); err != nil && !errors.Is(err, context.Canceled) {
		m.logger.Debug("Codex account capacity ensure degraded", "failure_category", "capacity_read")
	}
	if includeUsage && capabilities.UsageRead.State == domain.CodexCapabilitySupported {
		for _, record := range records {
			if record.Snapshot.Status == domain.CodexAccountStatusValid &&
				record.Snapshot.Authentication.State == domain.AgentAuthenticationAuthorized &&
				record.Snapshot.AuthMethod == domain.CodexAuthMethodChatGPT {
				_ = m.ensureUsage(ctx, record)
			}
		}
	}
	result, err := m.view(ids)
	m.publish()
	return result, err
}

func (m *codexAccountManager) ensureAuthentication(ctx context.Context, record codexAccountRecord, purpose domain.AgentReadinessPurpose, refreshToken bool) (domain.AgentAuthenticationObservation, error) {
	m.mu.Lock()
	state := m.auth[record.Snapshot.ID]
	if state == nil {
		state = &accountAuthState{invalidated: true}
		m.auth[record.Snapshot.ID] = state
	}
	current, _ := m.catalog.record(record.Snapshot.ID)
	ttl := codexAccountDisplayTTL
	if purpose == domain.AgentReadinessPurposeLaunch {
		ttl = codexAccountLaunchTTL
	}
	fresh := current.Snapshot.Authentication.CheckedAt != nil && m.now().Sub(*current.Snapshot.Authentication.CheckedAt) < ttl
	if !state.invalidated && fresh {
		out := current.Snapshot.Authentication
		m.mu.Unlock()
		return out, nil
	}
	if purpose == domain.AgentReadinessPurposeDisplay && !state.nextRetryAt.IsZero() && m.now().Before(state.nextRetryAt) {
		out := current.Snapshot.Authentication
		m.mu.Unlock()
		return out, nil
	}
	if state.call != nil {
		call := state.call
		m.mu.Unlock()
		select {
		case <-call.done:
			latest, _ := m.catalog.record(record.Snapshot.ID)
			return latest.Snapshot.Authentication, nil
		case <-ctx.Done():
			return domain.AgentAuthenticationObservation{}, ctx.Err()
		}
	}
	call := &accountAuthCall{done: make(chan struct{})}
	state.call = call
	m.catalog.updateSnapshot(record.Snapshot.ID, func(s *domain.CodexAccountSnapshot) { s.Authentication.Freshness = domain.AgentReadinessChecking })
	m.mu.Unlock()
	go m.runAuthentication(record, refreshToken, call)
	select {
	case <-call.done:
		latest, _ := m.catalog.record(record.Snapshot.ID)
		return latest.Snapshot.Authentication, nil
	case <-ctx.Done():
		return domain.AgentAuthenticationObservation{}, ctx.Err()
	}
}

func (m *codexAccountManager) runAuthentication(record codexAccountRecord, refresh bool, call *accountAuthCall) {
	attempted := m.now()
	select {
	case m.processes <- struct{}{}:
		defer func() { <-m.processes }()
	case <-m.ctx.Done():
		m.finishAuthentication(record.Snapshot.ID, failedAuthentication(attempted, domain.AgentReadinessReasonAuthCheckFailed, "Authentication check stopped."), domain.CodexAuthMethodUnknown, nil, true, call)
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, codexAccountAuthTimeout)
	defer cancel()
	client, err := m.factory.Open(ctx, m.accountContext(record))
	if err != nil {
		m.finishAuthentication(record.Snapshot.ID, failedAuthentication(attempted, domain.AgentReadinessReasonAuthCheckFailed, "Authentication check failed."), domain.CodexAuthMethodUnknown, nil, true, call)
		return
	}
	defer func() { _ = client.Close() }()
	observation, err := client.Read(ctx, refresh)
	if err != nil {
		code, reason := domain.AgentReadinessReasonAuthCheckFailed, "Authentication check failed."
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code, reason = domain.AgentReadinessReasonAuthCheckTimeout, "Authentication check timed out."
		}
		m.finishAuthentication(record.Snapshot.ID, failedAuthentication(attempted, code, reason), domain.CodexAuthMethodUnknown, nil, true, call)
		return
	}
	result := accountAuthenticationObservation(m.now(), observation.Authentication)
	m.finishAuthentication(record.Snapshot.ID, result, observation.Method, observation.Email, observation.Authentication == domain.AgentAuthenticationUnknown, call)
}

func accountAuthenticationObservation(at time.Time, state domain.AgentAuthenticationState) domain.AgentAuthenticationObservation {
	switch state {
	case domain.AgentAuthenticationAuthorized:
		return successfulAuthentication(at, state, domain.AgentReadinessReasonAuthorized, "Codex appears signed in.")
	case domain.AgentAuthenticationUnauthorized:
		return successfulAuthentication(at, state, domain.AgentReadinessReasonUnauthorized, "Codex needs authentication.")
	case domain.AgentAuthenticationNotApplicable:
		return successfulAuthentication(at, state, domain.AgentReadinessReasonAuthNotApplicable, "Codex authentication is not required.")
	default:
		return failedAuthentication(at, domain.AgentReadinessReasonAuthCheckInconclusive, "Authentication check was inconclusive.")
	}
}

func (m *codexAccountManager) finishAuthentication(id string, observation domain.AgentAuthenticationObservation, method domain.CodexAuthMethod, email *string, failed bool, call *accountAuthCall) {
	m.mu.Lock()
	state := m.auth[id]
	if failed {
		m.catalog.updateSnapshot(id, func(s *domain.CodexAccountSnapshot) { preserveAuthenticationFailure(&s.Authentication, observation) })
		state.invalidated = true
		state.failures++
		if state.failures <= len(defaultReadinessRetryDelays) {
			state.nextRetryAt = m.now().Add(defaultReadinessRetryDelays[state.failures-1])
		}
	} else {
		m.catalog.updateSnapshot(id, func(s *domain.CodexAccountSnapshot) {
			s.Authentication = observation
			s.AuthMethod = method
			s.AccountEmail = email
			s.Label = accountLabel(id, method, email)
		})
		state.invalidated = false
		state.failures = 0
		state.nextRetryAt = time.Time{}
	}
	state.call = nil
	close(call.done)
	m.mu.Unlock()
	m.publish()
}

func (m *codexAccountManager) invalidate(id string) {
	m.mu.Lock()
	state := m.auth[id]
	if state == nil {
		state = &accountAuthState{}
		m.auth[id] = state
	}
	state.invalidated = true
	state.nextRetryAt = time.Time{}
	m.mu.Unlock()
	m.catalog.updateSnapshot(id, func(s *domain.CodexAccountSnapshot) { s.Authentication.Freshness = domain.AgentReadinessStale })
	m.capacity.invalidate(id, true)
	m.publish()
}

func (m *codexAccountManager) ensureUsage(ctx context.Context, record codexAccountRecord) error {
	m.mu.Lock()
	state := m.usage[record.Snapshot.ID]
	if state == nil {
		state = &accountUsageState{}
		m.usage[record.Snapshot.ID] = state
	}
	if state.value != nil && m.now().Sub(state.checkedAt) < codexAccountUsageTTL {
		m.mu.Unlock()
		return nil
	}
	if state.call != nil {
		call := state.call
		m.mu.Unlock()
		select {
		case <-call:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	call := make(chan struct{})
	state.call = call
	m.mu.Unlock()
	go m.runUsage(record, state, call)
	select {
	case <-call:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *codexAccountManager) runUsage(record codexAccountRecord, state *accountUsageState, call chan struct{}) {
	defer func() {
		m.mu.Lock()
		if state.call == call {
			state.call = nil
			close(call)
		}
		m.mu.Unlock()
	}()
	select {
	case m.processes <- struct{}{}:
		defer func() { <-m.processes }()
	case <-m.ctx.Done():
		return
	}
	readCtx, cancel := context.WithTimeout(m.ctx, codexAccountAuthTimeout)
	defer cancel()
	client, err := m.factory.Open(readCtx, m.accountContext(record))
	if err != nil {
		return
	}
	defer func() { _ = client.Close() }()
	usage, err := client.ReadUsage(readCtx)
	if err != nil {
		return
	}
	value := &domain.CodexAccountUsageSummary{
		LatestDayTokens: usage.LatestDayTokens, LatestDayStartDate: usage.LatestDayStartDate,
		LifetimeTokens: usage.LifetimeTokens, CurrentStreakDays: usage.CurrentStreakDays,
		ObservedAt: usage.ObservedAt,
	}
	m.mu.Lock()
	state.value = value
	state.checkedAt = m.now()
	m.mu.Unlock()
	m.publish()
}

func (m *codexAccountManager) openLoginTerminal(ctx context.Context) (CodexAccountLoginTerminalStart, error) {
	if m.terminal == nil || m.executable == nil {
		return CodexAccountLoginTerminalStart{}, apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex login terminal is unavailable")
	}
	id := m.newID()
	now := m.now()
	snapshot := domain.CodexAccountLoginOperation{OperationID: id, Status: domain.CodexAccountLoginPending, ReasonCode: domain.CodexAccountLoginReasonPending, Reason: "Waiting for Codex sign-in.", ExpiresAt: now.Add(codexAccountLoginLifetime)}
	m.mu.Lock()
	if m.login != nil && !terminalLoginStatus(m.login.snapshot.Status) {
		m.mu.Unlock()
		return CodexAccountLoginTerminalStart{}, apierr.Conflict("CODEX_ACCOUNT_LOGIN_IN_PROGRESS", "A Codex account login is already in progress", nil)
	}
	previous := m.login
	m.login = &accountLoginOperation{snapshot: snapshot}
	m.mu.Unlock()
	if previous != nil {
		m.cleanupLoginFiles(previous)
	}
	pendingDir, home, err := createPendingCredentialHome(m.pendingRoot, id)
	if err != nil {
		m.clearLoginReservation(id)
		return CodexAccountLoginTerminalStart{}, apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex login could not be prepared")
	}
	executable, err := m.executable()
	if err != nil || strings.TrimSpace(executable) == "" {
		_ = os.RemoveAll(pendingDir)
		m.clearLoginReservation(id)
		return CodexAccountLoginTerminalStart{}, apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex login terminal is unavailable")
	}
	terminal, err := m.terminal.OpenCommandTerminal(ctx, shellterm.OpenCommandTerminalInput{Argv: []string{executable, "codex-login"}, Env: map[string]string{"CODEX_HOME": home}, WorkingDir: home, Title: "Add Codex account"})
	if err != nil {
		_ = os.RemoveAll(pendingDir)
		m.clearLoginReservation(id)
		return CodexAccountLoginTerminalStart{}, apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex login terminal could not be opened")
	}
	m.mu.Lock()
	if m.login == nil || m.login.snapshot.OperationID != id {
		m.mu.Unlock()
		_ = m.terminal.CloseShellTerminal(context.WithoutCancel(ctx), terminal.HandleID)
		_ = os.RemoveAll(pendingDir)
		return CodexAccountLoginTerminalStart{}, apierr.Conflict("CODEX_ACCOUNT_LOGIN_IN_PROGRESS", "A Codex account login changed concurrently", nil)
	}
	m.login.pendingDir, m.login.home, m.login.terminalHandle = pendingDir, home, terminal.HandleID
	m.mu.Unlock()
	go m.expireLogin(m.ctx, id, snapshot.ExpiresAt)
	m.publish()
	return CodexAccountLoginTerminalStart{Operation: snapshot, ShellTerminal: terminal}, nil
}

func (m *codexAccountManager) clearLoginReservation(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.login != nil && m.login.snapshot.OperationID == id {
		m.login = nil
	}
}

func (m *codexAccountManager) cleanupLoginFiles(op *accountLoginOperation) {
	if op == nil {
		return
	}
	if op.terminalHandle != "" && m.terminal != nil {
		_ = m.terminal.CloseShellTerminal(context.Background(), op.terminalHandle)
	}
	if op.pendingDir != "" {
		_ = os.RemoveAll(op.pendingDir)
	}
}

func terminalLoginStatus(status domain.CodexAccountLoginStatus) bool {
	return status == domain.CodexAccountLoginCompleted || status == domain.CodexAccountLoginCancelled || status == domain.CodexAccountLoginExpired || status == domain.CodexAccountLoginFailed
}

func (m *codexAccountManager) verifyLogin(ctx context.Context, operationID string) (domain.CodexAccountLoginOperation, error) {
	m.mu.Lock()
	op := m.login
	if op == nil || op.snapshot.OperationID != operationID {
		m.mu.Unlock()
		return domain.CodexAccountLoginOperation{}, apierr.NotFound("CODEX_ACCOUNT_LOGIN_NOT_FOUND", "Codex account login operation not found")
	}
	if terminalLoginStatus(op.snapshot.Status) {
		result := op.snapshot
		m.mu.Unlock()
		return result, nil
	}
	if op.closing || op.committing {
		result := op.snapshot
		m.mu.Unlock()
		return result, nil
	}
	if op.snapshot.Status == domain.CodexAccountLoginVerifying {
		result := op.snapshot
		m.mu.Unlock()
		return result, nil
	}
	op.snapshot.Status = domain.CodexAccountLoginVerifying
	op.snapshot.Reason = "Verifying the Codex account."
	home, pendingDir, terminalHandle := op.home, op.pendingDir, op.terminalHandle
	m.mu.Unlock()
	m.publish()
	select {
	case m.processes <- struct{}{}:
		defer func() { <-m.processes }()
	case <-m.ctx.Done():
		return domain.CodexAccountLoginOperation{}, m.ctx.Err()
	}
	verifyCtx, cancel := context.WithTimeout(m.ctx, codexAccountAuthTimeout)
	defer cancel()
	client, err := m.factory.Open(verifyCtx, ports.CodexAccountContext{Home: home, Managed: true})
	if err != nil {
		return m.finishLoginUnverified(operationID, "Codex could not verify this account."), nil
	}
	observation, readErr := client.Read(verifyCtx, true)
	_ = client.Close()
	if readErr != nil || observation.Authentication == domain.AgentAuthenticationUnknown {
		return m.finishLoginUnverified(operationID, "Codex could not verify this account."), nil
	}
	if observation.Authentication == domain.AgentAuthenticationUnauthorized {
		return m.finishLogin(operationID, domain.CodexAccountLoginUnauthorized, domain.CodexAccountLoginReasonUnauthorized, "Codex is still signed out.", nil), nil
	}
	if observation.Authentication != domain.AgentAuthenticationAuthorized && observation.Authentication != domain.AgentAuthenticationNotApplicable {
		return m.finishLoginUnverified(operationID, "Codex could not verify this account."), nil
	}
	m.mu.Lock()
	op = m.login
	if op == nil || op.snapshot.OperationID != operationID || terminalLoginStatus(op.snapshot.Status) || op.closing {
		var result domain.CodexAccountLoginOperation
		if op != nil && op.snapshot.OperationID == operationID {
			result = op.snapshot
		}
		m.mu.Unlock()
		return result, nil
	}
	op.committing = true
	op.commitDone = make(chan struct{})
	commitDone := op.commitDone
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if m.login != nil && m.login.snapshot.OperationID == operationID && m.login.commitDone == commitDone {
			m.login.committing = false
			close(commitDone)
			m.login.commitDone = nil
		}
		m.mu.Unlock()
	}()
	record, err := m.catalog.commitPending(pendingDir, observation)
	if err != nil {
		return m.finishLogin(operationID, domain.CodexAccountLoginFailed, domain.CodexAccountLoginReasonFailed, "The verified Codex account could not be saved.", nil), nil
	}
	m.catalog.updateSnapshot(record.Snapshot.ID, func(s *domain.CodexAccountSnapshot) {
		s.Authentication = accountAuthenticationObservation(m.now(), observation.Authentication)
		s.AuthMethod = observation.Method
		s.AccountEmail = observation.Email
		s.Label = accountLabel(s.ID, observation.Method, observation.Email)
	})
	m.mu.Lock()
	activateFirst := m.active.AccountID == "" && m.globalAuth.State == domain.AgentAuthenticationUnauthorized
	m.mu.Unlock()
	if activateFirst {
		m.mu.Lock()
		expectedRevision := m.active.Revision
		m.mu.Unlock()
		if err := m.activate(m.ctx, record.Snapshot.ID, expectedRevision); err != nil {
			result := m.finishLogin(operationID, domain.CodexAccountLoginFailed, domain.CodexAccountLoginReasonFailed, "The account was saved but could not be activated.", &record.Snapshot)
			if m.terminal != nil && terminalHandle != "" {
				_ = m.terminal.CloseShellTerminal(context.WithoutCancel(ctx), terminalHandle)
			}
			return result, nil
		}
	}
	latest, _ := m.catalog.record(record.Snapshot.ID)
	snapshot := latest.Snapshot
	snapshot.Active = snapshot.ID == m.activeAccountID()
	result := m.finishLogin(operationID, domain.CodexAccountLoginCompleted, domain.CodexAccountLoginReasonCompleted, "Codex account added.", &snapshot)
	if m.terminal != nil && terminalHandle != "" {
		_ = m.terminal.CloseShellTerminal(context.WithoutCancel(ctx), terminalHandle)
	}
	return result, nil
}

func (m *codexAccountManager) finishLoginUnverified(id, reason string) domain.CodexAccountLoginOperation {
	return m.finishLogin(id, domain.CodexAccountLoginUnverified, domain.CodexAccountLoginReasonUnverified, reason, nil)
}
func (m *codexAccountManager) finishLogin(id string, status domain.CodexAccountLoginStatus, code, reason string, account *domain.CodexAccountSnapshot) domain.CodexAccountLoginOperation {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.login == nil || m.login.snapshot.OperationID != id {
		return domain.CodexAccountLoginOperation{}
	}
	if m.login.closing && status != domain.CodexAccountLoginCancelled && status != domain.CodexAccountLoginExpired {
		return m.login.snapshot
	}
	if terminalLoginStatus(m.login.snapshot.Status) && m.login.snapshot.Status != status {
		return m.login.snapshot
	}
	m.login.snapshot.Status, m.login.snapshot.ReasonCode, m.login.snapshot.Reason, m.login.snapshot.Account = status, code, reason, account
	result := m.login.snapshot
	go m.publish()
	return result
}

func (m *codexAccountManager) cancelLogin(ctx context.Context, operationID string) (domain.CodexAccountLoginOperation, error) {
	for {
		m.mu.Lock()
		op := m.login
		if op == nil || op.snapshot.OperationID != operationID {
			m.mu.Unlock()
			return domain.CodexAccountLoginOperation{}, apierr.NotFound("CODEX_ACCOUNT_LOGIN_NOT_FOUND", "Codex account login operation not found")
		}
		if terminalLoginStatus(op.snapshot.Status) {
			result := op.snapshot
			m.mu.Unlock()
			return result, nil
		}
		if op.committing && op.commitDone != nil {
			done := op.commitDone
			m.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return domain.CodexAccountLoginOperation{}, ctx.Err()
			}
		}
		if op.closing {
			result := op.snapshot
			m.mu.Unlock()
			return result, nil
		}
		op.closing = true
		handle, pending := op.terminalHandle, op.pendingDir
		m.mu.Unlock()
		if handle != "" && m.terminal != nil {
			if err := m.terminal.CloseShellTerminal(ctx, handle); err != nil {
				m.mu.Lock()
				if m.login != nil && m.login.snapshot.OperationID == operationID {
					m.login.closing = false
					m.login.snapshot.Status = domain.CodexAccountLoginUnverified
					m.login.snapshot.ReasonCode = domain.CodexAccountLoginReasonUnverified
					m.login.snapshot.Reason = "Codex login terminal could not be closed."
				}
				m.mu.Unlock()
				m.publish()
				return domain.CodexAccountLoginOperation{}, apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex login terminal could not be closed")
			}
		}
		_ = os.RemoveAll(pending)
		return m.finishLogin(operationID, domain.CodexAccountLoginCancelled, domain.CodexAccountLoginReasonCancelled, "Codex account login was cancelled.", nil), nil
	}
}

func (m *codexAccountManager) expireLogin(ctx context.Context, id string, at time.Time) {
	timer := time.NewTimer(time.Until(at))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-m.ctx.Done():
		return
	}
	for {
		m.mu.Lock()
		op := m.login
		if op == nil || op.snapshot.OperationID != id || terminalLoginStatus(op.snapshot.Status) || op.closing {
			m.mu.Unlock()
			return
		}
		if op.committing && op.commitDone != nil {
			done := op.commitDone
			m.mu.Unlock()
			select {
			case <-done:
				continue
			case <-m.ctx.Done():
				return
			}
		}
		op.closing = true
		pending, handle := op.pendingDir, op.terminalHandle
		m.mu.Unlock()
		if pending == "" {
			return
		}
		if handle != "" && m.terminal != nil {
			_ = m.terminal.CloseShellTerminal(ctx, handle)
		}
		_ = os.RemoveAll(pending)
		m.finishLogin(id, domain.CodexAccountLoginExpired, domain.CodexAccountLoginReasonExpired, "Codex account login expired.", nil)
		return
	}
}

func (m *codexAccountManager) activeAccountID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active.AccountID
}

func (m *codexAccountManager) activate(ctx context.Context, accountID string, expectedRevision int64) error {
	record, ok := m.catalog.record(accountID)
	if !ok || record.Snapshot.Status != domain.CodexAccountStatusValid {
		return apierr.NotFound("CODEX_ACCOUNT_NOT_FOUND", "Codex account not found")
	}
	_, err := m.activateFromCredential(ctx, accountID, expectedRevision, filepath.Join(record.Home, codexCredentialFilename), nil)
	return err
}

func (m *codexAccountManager) activateFromCredential(ctx context.Context, accountID string, expectedRevision int64, sourceCredential string, expectedGlobal []byte) (domain.CodexActiveAccount, error) {
	record, ok := m.catalog.record(accountID)
	if !ok || record.Snapshot.Status != domain.CodexAccountStatusValid {
		return domain.CodexActiveAccount{}, apierr.NotFound("CODEX_ACCOUNT_NOT_FOUND", "Codex account not found")
	}
	targetCredential, err := readOpaqueCredential(sourceCredential)
	if err != nil {
		return domain.CodexActiveAccount{}, err
	}
	globalPath := m.globalCredentialPath()
	previousCredential, previousErr := readOpaqueCredential(globalPath)
	if previousErr != nil && !errors.Is(previousErr, os.ErrNotExist) {
		return domain.CodexActiveAccount{}, ports.ErrCodexGlobalCredentialStoreUnsupported
	}
	if expectedGlobal != nil && (previousErr != nil || !bytes.Equal(previousCredential, expectedGlobal)) {
		return domain.CodexActiveAccount{}, ports.ErrCodexGlobalAccountChanged
	}
	restorePrevious := func() error {
		current, currentErr := readOpaqueCredential(globalPath)
		if currentErr != nil || !bytes.Equal(current, targetCredential) {
			return ports.ErrCodexGlobalAccountChanged
		}
		if previousErr == nil {
			return writeGlobalCredentialAtomic(globalPath, previousCredential)
		}
		return removeGlobalCredential(globalPath)
	}
	if err := writeGlobalCredentialAtomic(globalPath, targetCredential); err != nil {
		return domain.CodexActiveAccount{}, err
	}
	verifyCtx, cancel := context.WithTimeout(ctx, codexAccountAuthTimeout)
	defer cancel()
	client, err := m.factory.Open(verifyCtx, ports.CodexAccountContext{Home: m.globalHome, Managed: false})
	if err != nil {
		if restoreErr := restorePrevious(); restoreErr != nil {
			return domain.CodexActiveAccount{}, restoreErr
		}
		return domain.CodexActiveAccount{}, err
	}
	observation, err := client.Read(verifyCtx, true)
	_ = client.Close()
	if err != nil || (observation.Authentication != domain.AgentAuthenticationAuthorized && observation.Authentication != domain.AgentAuthenticationNotApplicable) || !codexObservationMatchesAccount(record.Snapshot, observation) {
		currentCredential, currentErr := readOpaqueCredential(globalPath)
		if currentErr != nil || !bytes.Equal(currentCredential, targetCredential) {
			return domain.CodexActiveAccount{}, ports.ErrCodexGlobalAccountChanged
		}
		if restoreErr := restorePrevious(); restoreErr != nil {
			return domain.CodexActiveAccount{}, restoreErr
		}
		return domain.CodexActiveAccount{}, apierr.Conflict("CODEX_ACCOUNT_AUTH_UNVERIFIED", "Codex could not verify the selected account", nil)
	}
	now := m.now()
	var active domain.CodexActiveAccount
	if m.stateStore != nil {
		active, err = m.stateStore.SetCodexActiveAccount(ctx, accountID, expectedRevision, now)
		if err != nil {
			if restoreErr := restorePrevious(); restoreErr != nil {
				return domain.CodexActiveAccount{}, restoreErr
			}
			return domain.CodexActiveAccount{}, err
		}
	} else {
		active = domain.CodexActiveAccount{AccountID: accountID, Revision: expectedRevision + 1, ActivatedAt: now, UpdatedAt: now}
	}
	m.mu.Lock()
	m.active = active
	m.unmanaged = nil
	m.mu.Unlock()
	if refreshed, readErr := readOpaqueCredential(globalPath); readErr == nil {
		_ = writePrivateFileAtomic(filepath.Join(record.Home, codexCredentialFilename), refreshed)
	}
	m.invalidate(accountID)
	m.publish()
	return active, nil
}

func removeGlobalCredential(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(info) || !hasSingleHardLink(info) {
		return errors.New("global Codex credential is not a safe regular file")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func readOpaqueCredential(source string) ([]byte, error) {
	info, err := os.Lstat(source)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > 8<<20 || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) || !hasSingleHardLink(info) {
		return nil, errors.New("codex credential is not a safe private regular file")
	}
	f, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("codex credential changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, 8<<20))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != info.Size() {
		return nil, errors.New("codex credential size changed while copying")
	}
	return data, nil
}

func copyOpaqueCredential(source, target string) error {
	data, err := readOpaqueCredential(source)
	if err != nil {
		return err
	}
	return writePrivateFileAtomic(target, data)
}

func writeGlobalCredentialAtomic(path string, data []byte) error {
	if len(data) == 0 || len(data) > 8<<20 {
		return errors.New("global Codex credential is empty or too large")
	}
	parent := filepath.Dir(path)
	info, err := os.Lstat(parent)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(parent)
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(info) {
		return ports.ErrCodexGlobalCredentialStoreUnsupported
	}
	temp, err := os.CreateTemp(parent, ".ao-auth-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	removeTemp := true
	defer func() {
		_ = temp.Close()
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := replaceCodexFile(tempPath, path); err != nil {
		return err
	}
	removeTemp = false
	return syncDirectory(parent)
}

func (m *codexAccountManager) globalCredentialPath() string {
	return filepath.Join(m.globalHome, codexCredentialFilename)
}

func (m *codexAccountManager) validateGlobalCredentialStore() error {
	info, err := os.Lstat(m.globalHome)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(info) {
		return ports.ErrCodexGlobalCredentialStoreUnsupported
	}
	_, err = readOpaqueCredential(m.globalCredentialPath())
	if err != nil {
		return ports.ErrCodexGlobalCredentialStoreUnsupported
	}
	return nil
}

func (m *codexAccountManager) bootstrap() {
	m.bootstrapOnce.Do(func() {
		defer close(m.bootstrapDone)
		err := m.bootstrapInner()
		m.mu.Lock()
		m.bootstrapErr = err
		m.bootstrapped = err == nil
		m.mu.Unlock()
		m.markActive()
		m.publish()
	})
}

func (m *codexAccountManager) bootstrapInner() error {
	if err := cleanupPendingCredentialHomes(m.pendingRoot); err != nil {
		return err
	}
	if err := cleanupPendingCredentialHomes(m.switchStagingRoot); err != nil {
		return err
	}
	if err := m.catalog.refresh(); err != nil {
		return err
	}
	if m.stateStore != nil {
		active, ok, err := m.stateStore.GetCodexActiveAccount(m.ctx)
		if err != nil {
			return err
		}
		if ok {
			m.mu.Lock()
			m.active = active
			m.mu.Unlock()
		}
	}
	return m.reconcileGlobal(m.ctx)
}

func (m *codexAccountManager) reconcileGlobal(ctx context.Context) error {
	m.mu.Lock()
	call := m.reconcile
	if call == nil {
		call = &accountReconcileCall{done: make(chan struct{})}
		m.reconcile = call
		go m.runGlobalReconciliation(call)
	}
	m.mu.Unlock()
	select {
	case <-call.done:
		return call.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *codexAccountManager) runGlobalReconciliation(call *accountReconcileCall) {
	call.err = m.reconcileGlobalInner()
	m.mu.Lock()
	if m.reconcile == call {
		m.reconcile = nil
	}
	close(call.done)
	m.mu.Unlock()
	m.markActive()
	m.publish()
}

func (m *codexAccountManager) reconcileGlobalInner() error {
	if m.factory == nil || m.globalHome == "" {
		return errors.New("codex global account discovery is unavailable")
	}
	select {
	case m.processes <- struct{}{}:
		defer func() { <-m.processes }()
	case <-m.ctx.Done():
		return m.ctx.Err()
	}
	readCtx, cancel := context.WithTimeout(m.ctx, codexAccountAuthTimeout)
	defer cancel()
	client, err := m.factory.Open(readCtx, ports.CodexAccountContext{Home: m.globalHome, Managed: false})
	if err != nil {
		m.setGlobalAuthenticationFailure(failedAuthentication(m.now(), domain.AgentReadinessReasonAuthCheckFailed, "Authentication check failed."))
		m.setUnmanagedGlobal("Device Codex account", domain.CodexAuthMethodUnknown, nil, "global_account_unverified", "AO could not verify the device's current Codex account.")
		return m.setActivePointer(m.ctx, "")
	}
	observation, readErr := client.Read(readCtx, false)
	_ = client.Close()
	if readErr != nil || observation.Authentication == domain.AgentAuthenticationUnknown {
		m.setGlobalAuthenticationFailure(failedAuthentication(m.now(), domain.AgentReadinessReasonAuthCheckInconclusive, "Authentication check was inconclusive."))
		m.setUnmanagedGlobal("Device Codex account", observation.Method, observation.Email, "global_account_unverified", "AO could not verify the device's current Codex account.")
		return m.setActivePointer(m.ctx, "")
	}
	if observation.Authentication == domain.AgentAuthenticationUnauthorized {
		m.setGlobalAuthentication(accountAuthenticationObservation(m.now(), observation.Authentication))
		m.mu.Lock()
		m.unmanaged = nil
		m.mu.Unlock()
		return m.setActivePointer(m.ctx, "")
	}
	if observation.Authentication != domain.AgentAuthenticationAuthorized && observation.Authentication != domain.AgentAuthenticationNotApplicable {
		m.setGlobalAuthenticationFailure(failedAuthentication(m.now(), domain.AgentReadinessReasonAuthCheckInconclusive, "Authentication check was inconclusive."))
		m.setUnmanagedGlobal("Device Codex account", observation.Method, observation.Email, "global_account_unverified", "AO could not verify the device's current Codex account.")
		return nil
	}
	m.setGlobalAuthentication(accountAuthenticationObservation(m.now(), observation.Authentication))
	globalCredential, credentialErr := readOpaqueCredential(m.globalCredentialPath())
	if credentialErr != nil || m.validateGlobalCredentialStore() != nil {
		m.setUnmanagedGlobal(accountLabel("device", observation.Method, observation.Email), observation.Method, observation.Email, "global_credential_store_unsupported", "This Codex account is active on the device, but its credential store cannot be switched safely.")
		return m.setActivePointer(m.ctx, "")
	}
	credentialObservation, credentialErr := m.verifyOpaqueGlobalCredential(globalCredential)
	if credentialErr != nil || !codexObservationsMatch(observation, credentialObservation) {
		m.setUnmanagedGlobal(accountLabel("device", observation.Method, observation.Email), observation.Method, observation.Email, "global_credential_store_unsupported", "This Codex account is active on the device, but its credential store cannot be switched safely.")
		return m.setActivePointer(m.ctx, "")
	}
	observation = credentialObservation
	if err := m.catalog.refresh(); err != nil {
		return err
	}
	record, found := m.matchGlobalAccount(observation, globalCredential)
	if !found {
		if !distinguishableCodexIdentity(observation) {
			m.setUnmanagedGlobal(accountLabel("device", observation.Method, observation.Email), observation.Method, observation.Email, "global_account_identity_unverified", "AO cannot safely distinguish this device Codex account from saved accounts.")
			return m.setActivePointer(m.ctx, "")
		}
		pendingID := m.newID()
		pendingDir, home, createErr := createPendingCredentialHome(m.pendingRoot, pendingID)
		if createErr != nil {
			return createErr
		}
		defer func() { _ = os.RemoveAll(pendingDir) }()
		if err := writePrivateFileAtomic(filepath.Join(home, codexCredentialFilename), globalCredential); err != nil {
			return err
		}
		verifyCtx, verifyCancel := context.WithTimeout(m.ctx, codexAccountAuthTimeout)
		verifiedClient, openErr := m.factory.Open(verifyCtx, ports.CodexAccountContext{Home: home, Managed: true})
		if openErr != nil {
			verifyCancel()
			return openErr
		}
		checked, checkErr := verifiedClient.Read(verifyCtx, true)
		_ = verifiedClient.Close()
		verifyCancel()
		if checkErr != nil || (checked.Authentication != domain.AgentAuthenticationAuthorized && checked.Authentication != domain.AgentAuthenticationNotApplicable) {
			m.setUnmanagedGlobal(accountLabel("device", observation.Method, observation.Email), observation.Method, observation.Email, "global_account_unverified", "AO could not verify the device's current Codex account for import.")
			return m.setActivePointer(m.ctx, "")
		}
		record, err = m.catalog.commitPending(pendingDir, checked)
		if err != nil {
			return err
		}
		observation = checked
	}
	if err := writePrivateFileAtomic(filepath.Join(record.Home, codexCredentialFilename), globalCredential); err != nil {
		return err
	}
	if err := m.catalog.updateVerifiedDescriptor(record.Snapshot.ID, observation); err != nil {
		return err
	}
	if latestGlobal, latestErr := readOpaqueCredential(m.globalCredentialPath()); latestErr != nil || !bytes.Equal(latestGlobal, globalCredential) {
		m.setUnmanagedGlobal(accountLabel("device", observation.Method, observation.Email), observation.Method, observation.Email, "global_account_changed", "The device Codex account changed while AO was reconciling it.")
		_ = m.setActivePointer(m.ctx, "")
		return ports.ErrCodexGlobalAccountChanged
	}
	m.catalog.updateSnapshot(record.Snapshot.ID, func(snapshot *domain.CodexAccountSnapshot) {
		snapshot.Authentication = accountAuthenticationObservation(m.now(), observation.Authentication)
	})
	if err := m.setActivePointer(m.ctx, record.Snapshot.ID); err != nil {
		return err
	}
	m.mu.Lock()
	m.unmanaged = nil
	m.mu.Unlock()
	return nil
}

func (m *codexAccountManager) matchGlobalAccount(observation ports.CodexAccountObservation, globalCredential []byte) (codexAccountRecord, bool) {
	records, err := m.catalog.recordsFor(nil)
	if err != nil {
		return codexAccountRecord{}, false
	}
	m.mu.Lock()
	activeID := m.active.AccountID
	m.mu.Unlock()
	if active, ok := m.catalog.record(activeID); ok && active.Snapshot.Status == domain.CodexAccountStatusValid {
		if sameCodexStructuredIdentity(active.Snapshot, observation) || credentialMatchesRecord(active, globalCredential) {
			return active, true
		}
	}
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].Snapshot.Status == domain.CodexAccountStatusValid && sameCodexStructuredIdentity(records[i].Snapshot, observation) {
			return records[i], true
		}
	}
	return codexAccountRecord{}, false
}

func credentialMatchesRecord(record codexAccountRecord, credential []byte) bool {
	stored, err := readOpaqueCredential(filepath.Join(record.Home, codexCredentialFilename))
	return err == nil && bytes.Equal(stored, credential)
}

func distinguishableCodexIdentity(observation ports.CodexAccountObservation) bool {
	return observation.Method != domain.CodexAuthMethodUnknown && observation.Email != nil && safeAccountEmail(*observation.Email)
}

func sameCodexStructuredIdentity(snapshot domain.CodexAccountSnapshot, observation ports.CodexAccountObservation) bool {
	if snapshot.AuthMethod != observation.Method || !distinguishableCodexIdentity(observation) || snapshot.AccountEmail == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(*snapshot.AccountEmail), strings.TrimSpace(*observation.Email))
}

func codexObservationMatchesAccount(snapshot domain.CodexAccountSnapshot, observation ports.CodexAccountObservation) bool {
	if sameCodexStructuredIdentity(snapshot, observation) {
		return true
	}
	return snapshot.AuthMethod == domain.CodexAuthMethodAPIKey && observation.Method == domain.CodexAuthMethodAPIKey
}

func codexObservationsMatch(left, right ports.CodexAccountObservation) bool {
	if left.Method != right.Method {
		return false
	}
	if left.Email != nil && right.Email != nil && safeAccountEmail(*left.Email) && safeAccountEmail(*right.Email) {
		return strings.EqualFold(strings.TrimSpace(*left.Email), strings.TrimSpace(*right.Email))
	}
	return left.Method == domain.CodexAuthMethodAPIKey
}

func (m *codexAccountManager) verifyOpaqueGlobalCredential(credential []byte) (ports.CodexAccountObservation, error) {
	pendingID := m.newID()
	pendingDir, home, err := createPendingCredentialHome(m.pendingRoot, pendingID)
	if err != nil {
		return ports.CodexAccountObservation{}, err
	}
	defer func() { _ = os.RemoveAll(pendingDir) }()
	if err := writePrivateFileAtomic(filepath.Join(home, codexCredentialFilename), credential); err != nil {
		return ports.CodexAccountObservation{}, err
	}
	verifyCtx, cancel := context.WithTimeout(m.ctx, codexAccountAuthTimeout)
	defer cancel()
	client, err := m.factory.Open(verifyCtx, ports.CodexAccountContext{Home: home, Managed: true})
	if err != nil {
		return ports.CodexAccountObservation{}, err
	}
	// Reconciliation only needs to prove that the opaque global credential is
	// usable from a file-backed home. A proactive refresh here can race Codex's
	// live global credential and rotate the copied refresh token, incorrectly
	// classifying the device account as unmanaged. Strict refresh remains part
	// of login and switch admission, where no duplicate live credential is being
	// introduced.
	observation, readErr := client.Read(verifyCtx, false)
	_ = client.Close()
	if readErr != nil {
		return ports.CodexAccountObservation{}, readErr
	}
	if observation.Authentication != domain.AgentAuthenticationAuthorized && observation.Authentication != domain.AgentAuthenticationNotApplicable {
		return ports.CodexAccountObservation{}, errors.New("global Codex credential could not be verified in a file-backed store")
	}
	return observation, nil
}

func (m *codexAccountManager) setUnmanagedGlobal(label string, method domain.CodexAuthMethod, email *string, code, reason string) {
	m.mu.Lock()
	m.unmanaged = &domain.CodexUnmanagedGlobalAccount{Label: label, AuthMethod: method, AccountEmail: email, ReasonCode: code, Reason: reason}
	m.mu.Unlock()
}

func (m *codexAccountManager) setGlobalAuthentication(observation domain.AgentAuthenticationObservation) {
	m.mu.Lock()
	m.globalAuth = observation
	m.mu.Unlock()
}

func (m *codexAccountManager) setGlobalAuthenticationFailure(observation domain.AgentAuthenticationObservation) {
	m.mu.Lock()
	preserveAuthenticationFailure(&m.globalAuth, observation)
	m.mu.Unlock()
}

func (m *codexAccountManager) setActivePointer(ctx context.Context, accountID string) error {
	m.mu.Lock()
	current := m.active
	m.mu.Unlock()
	if current.AccountID == accountID {
		return nil
	}
	now := m.now()
	var (
		active domain.CodexActiveAccount
		err    error
	)
	if m.stateStore != nil {
		active, err = m.stateStore.SetCodexActiveAccount(ctx, accountID, current.Revision, now)
		if err != nil {
			return err
		}
	} else {
		active = domain.CodexActiveAccount{AccountID: accountID, Revision: current.Revision + 1, ActivatedAt: now, UpdatedAt: now}
	}
	m.mu.Lock()
	m.active = active
	m.mu.Unlock()
	return nil
}

func (m *codexAccountManager) markActive() {
	id := m.activeAccountID()
	for _, s := range m.catalog.snapshots() {
		m.catalog.updateSnapshot(s.ID, func(snapshot *domain.CodexAccountSnapshot) { snapshot.Active = snapshot.ID == id })
	}
}

func mapUnknownCodexAccount(err error) error {
	var unknown unknownCodexAccountError
	if errors.As(err, &unknown) {
		return apierr.Invalid("INVALID_CODEX_ACCOUNT_ID", "Unknown Codex account", map[string]any{"accountId": unknown.id})
	}
	return err
}

func (m *codexAccountManager) subscribe(ctx context.Context) <-chan CodexAccounts {
	ch := make(chan CodexAccounts, 1)
	m.mu.Lock()
	m.subscribers[ch] = struct{}{}
	m.mu.Unlock()
	ch <- m.cached()
	go func() { <-ctx.Done(); m.mu.Lock(); delete(m.subscribers, ch); close(ch); m.mu.Unlock() }()
	return ch
}
func (m *codexAccountManager) publish() {
	snapshot := m.cached()
	m.mu.Lock()
	defer m.mu.Unlock()
	for ch := range m.subscribers {
		select {
		case ch <- snapshot:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- snapshot:
			default:
			}
		}
	}
}

// Service integration.
func (s *Service) structuredCodexAuthentication(ctx context.Context, agentID string, purpose domain.AgentReadinessPurpose) (domain.AgentAuthenticationObservation, bool) {
	if agentID != string(domain.HarnessCodex) || s.codexAccounts == nil || s.codexAccounts.factory == nil {
		return domain.AgentAuthenticationObservation{}, false
	}
	if purpose == domain.AgentReadinessPurposeLaunch {
		if err := s.WaitCodexAccountBootstrap(ctx); err != nil {
			return failedAuthentication(s.codexAccounts.now(), domain.AgentReadinessReasonAuthCheckFailed, "Codex account setup did not complete."), true
		}
	} else {
		s.codexAccounts.mu.Lock()
		bootstrapped := s.codexAccounts.bootstrapped
		s.codexAccounts.mu.Unlock()
		if !bootstrapped {
			return domain.AgentAuthenticationObservation{}, false
		}
	}
	id := s.codexAccounts.activeAccountID()
	if id == "" {
		s.codexAccounts.mu.Lock()
		global := s.codexAccounts.globalAuth
		s.codexAccounts.mu.Unlock()
		if global.State == domain.AgentAuthenticationAuthorized || global.State == domain.AgentAuthenticationNotApplicable || global.State == domain.AgentAuthenticationUnknown {
			return global, true
		}
		return successfulAuthentication(s.codexAccounts.now(), domain.AgentAuthenticationUnauthorized, domain.AgentReadinessReasonUnauthorized, "Sign in to Codex or add an account in Settings."), true
	}
	record, ok := s.codexAccounts.catalog.record(id)
	if !ok {
		return failedAuthentication(s.codexAccounts.now(), domain.AgentReadinessReasonAuthCheckInconclusive, "The active Codex account is unavailable."), true
	}
	result, err := s.codexAccounts.ensureAuthentication(ctx, record, purpose, purpose == domain.AgentReadinessPurposeLaunch)
	if err != nil {
		return failedAuthentication(s.codexAccounts.now(), domain.AgentReadinessReasonAuthCheckFailed, "Authentication check failed."), true
	}
	return result, true
}

// CachedCodexAccounts returns the current in-memory view without native work.
func (s *Service) CachedCodexAccounts(ctx context.Context) (CodexAccounts, error) {
	if err := ctx.Err(); err != nil {
		return CodexAccounts{}, err
	}
	if s.codexAccounts == nil {
		return CodexAccounts{}, apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex account management is unavailable")
	}
	result := s.codexAccounts.cached()
	if s.codexSwitches != nil {
		if sw, ok, err := s.codexSwitches.GetActiveCodexAccountSwitch(ctx); err == nil && ok {
			result.CurrentSwitch = &sw
		}
	}
	return result, nil
}

// EnsureCodexAccounts rediscovers requested accounts and refreshes eligible observations.
func (s *Service) EnsureCodexAccounts(ctx context.Context, ids []string, includeUsage bool) (CodexAccounts, error) {
	if s.codexAccounts == nil {
		return CodexAccounts{}, apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex account management is unavailable")
	}
	if s.codexSwitches != nil && s.codexSwitches.CodexAccountSwitchInProgress() {
		if sw, ok, err := s.codexSwitches.GetActiveCodexAccountSwitch(ctx); err == nil && ok {
			result := s.codexAccounts.cached()
			result.CurrentSwitch = &sw
			return result, nil
		}
		return s.codexAccounts.cached(), nil
	}
	if err := s.WaitCodexAccountBootstrap(ctx); err != nil {
		return CodexAccounts{}, err
	}
	installation, err := s.readiness.EnsureInstallation(ctx, []string{string(domain.HarnessCodex)}, domain.AgentReadinessPurposeDisplay)
	if err != nil {
		return CodexAccounts{}, err
	}
	result, err := s.codexAccounts.ensure(ctx, ids, includeUsage, installation[0].Installation.State)
	if err == nil && s.codexSwitches != nil {
		if sw, ok, switchErr := s.codexSwitches.GetActiveCodexAccountSwitch(ctx); switchErr == nil && ok {
			result.CurrentSwitch = &sw
		}
	}
	return result, err
}

// SubscribeCodexAccounts returns cached state followed by latest-wins updates.
func (s *Service) SubscribeCodexAccounts(ctx context.Context) (<-chan CodexAccounts, error) {
	if s.codexAccounts == nil {
		return nil, apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex account management is unavailable")
	}
	source := s.codexAccounts.subscribe(ctx)
	out := make(chan CodexAccounts, 1)
	go func() {
		defer close(out)
		for snapshot := range source {
			if s.codexSwitches != nil {
				if sw, ok, err := s.codexSwitches.GetActiveCodexAccountSwitch(ctx); err == nil && ok {
					snapshot.CurrentSwitch = &sw
				}
			}
			select {
			case out <- snapshot:
			default:
				select {
				case <-out:
				default:
				}
				select {
				case out <- snapshot:
				default:
				}
			}
		}
	}()
	return out, nil
}

// PublishCodexAccounts notifies subscribers after externally owned switch changes.
func (s *Service) PublishCodexAccounts() {
	if s.codexAccounts != nil {
		s.codexAccounts.publish()
	}
}

// SetCodexAccountLoginTerminalOpener wires the trusted shell-terminal boundary.
func (s *Service) SetCodexAccountLoginTerminalOpener(opener codexAccountLoginTerminalService) {
	if s.codexAccounts != nil {
		s.codexAccounts.terminal = opener
	}
}

// OpenCodexAccountLoginTerminal starts one private native-login operation.
func (s *Service) OpenCodexAccountLoginTerminal(ctx context.Context) (CodexAccountLoginTerminalStart, error) {
	if s.codexAccounts == nil {
		return CodexAccountLoginTerminalStart{}, apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex account management is unavailable")
	}
	if s.codexSwitches != nil && s.codexSwitches.CodexAccountSwitchInProgress() {
		return CodexAccountLoginTerminalStart{}, apierr.Conflict("CODEX_ACCOUNT_SWITCH_IN_PROGRESS", "A Codex account switch is already in progress", nil)
	}
	if err := s.WaitCodexAccountBootstrap(ctx); err != nil {
		return CodexAccountLoginTerminalStart{}, err
	}
	if err := s.requireCodexAccountInstallation(ctx); err != nil {
		return CodexAccountLoginTerminalStart{}, err
	}
	capabilities := s.codexAccounts.detectCapabilities(ctx)
	switch capabilities.AccountManagement.State {
	case domain.CodexCapabilityUnsupported:
		return CodexAccountLoginTerminalStart{}, apierr.NotImplemented("CODEX_ACCOUNT_MANAGEMENT_UNSUPPORTED", "This Codex version does not support account management")
	case domain.CodexCapabilityUnknown:
		return CodexAccountLoginTerminalStart{}, apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex account management capability could not be verified")
	}
	return s.codexAccounts.openLoginTerminal(ctx)
}

// VerifyCodexAccountLogin verifies one pending login through structured account read.
func (s *Service) VerifyCodexAccountLogin(ctx context.Context, operationID string) (domain.CodexAccountLoginOperation, error) {
	if s.codexAccounts == nil {
		return domain.CodexAccountLoginOperation{}, apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex account management is unavailable")
	}
	return s.codexAccounts.verifyLogin(ctx, strings.TrimSpace(operationID))
}

// CancelCodexAccountLogin destroys a pending login and its credential staging.
func (s *Service) CancelCodexAccountLogin(ctx context.Context, operationID string) (domain.CodexAccountLoginOperation, error) {
	if s.codexAccounts == nil {
		return domain.CodexAccountLoginOperation{}, apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex account management is unavailable")
	}
	return s.codexAccounts.cancelLogin(ctx, strings.TrimSpace(operationID))
}
func (s *Service) requireCodexAccountInstallation(ctx context.Context) error {
	observations, err := s.readiness.EnsureInstallation(ctx, []string{string(domain.HarnessCodex)}, domain.AgentReadinessPurposeDisplay)
	if err != nil {
		return err
	}
	if observations[0].Installation.State == domain.AgentInstallationNotInstalled && observations[0].Installation.Freshness == domain.AgentReadinessFresh {
		return apierr.NotImplemented("CODEX_ACCOUNT_MANAGEMENT_UNSUPPORTED", "Codex is not installed")
	}
	return nil
}

// InvalidateCodexAccountAuthentication invalidates the globally active account.
func (s *Service) InvalidateCodexAccountAuthentication() {
	if s.codexAccounts == nil {
		return
	}
	id := s.codexAccounts.activeAccountID()
	if id != "" {
		s.codexAccounts.invalidate(id)
	}
	s.readiness.Invalidate(string(domain.HarnessCodex), readinessInvalidateAuthentication)
	go func() { _ = s.codexAccounts.reconcileGlobal(s.codexAccounts.ctx) }()
}

// ObserveActiveCodexAccountCapacity attributes a provider event to the active account.
func (s *Service) ObserveActiveCodexAccountCapacity(observation ports.CodexCapacityObservation) {
	if s.codexAccounts == nil {
		return
	}
	id := s.codexAccounts.activeAccountID()
	if id != "" {
		s.codexAccounts.capacity.updateFromEvent(id, observation)
	}
}

// WarmCodexAccounts starts asynchronous bootstrap and observation warming.
func (s *Service) WarmCodexAccounts() {
	if s.codexAccounts == nil {
		return
	}
	go func() {
		s.codexAccounts.bootstrap()
		select {
		case <-s.codexAccounts.bootstrapDone:
		case <-s.codexAccounts.ctx.Done():
			return
		}
		s.codexAccounts.mu.Lock()
		bootstrapErr := s.codexAccounts.bootstrapErr
		s.codexAccounts.mu.Unlock()
		if bootstrapErr != nil {
			return
		}
		capabilities := s.codexAccounts.detectCapabilities(s.codexAccounts.ctx)
		records, err := s.codexAccounts.catalog.recordsFor(nil)
		if err != nil {
			return
		}
		if capabilities.AccountRead.State == domain.CodexCapabilitySupported {
			for _, record := range records {
				if record.Snapshot.Status == domain.CodexAccountStatusValid {
					_, _ = s.codexAccounts.ensureAuthentication(s.codexAccounts.ctx, record, domain.AgentReadinessPurposeDisplay, false)
				}
			}
		}
		_ = s.codexAccounts.capacity.ensure(s.codexAccounts.ctx, records, capabilities)
	}()
}

// WaitCodexAccountBootstrap waits for the daemon-owned bootstrap gate.
func (s *Service) WaitCodexAccountBootstrap(ctx context.Context) error {
	if s.codexAccounts == nil {
		return apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex account management is unavailable")
	}
	go s.codexAccounts.bootstrap()
	select {
	case <-s.codexAccounts.bootstrapDone:
		s.codexAccounts.mu.Lock()
		err := s.codexAccounts.bootstrapErr
		s.codexAccounts.mu.Unlock()
		if err != nil {
			return apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex account setup did not complete")
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// CurrentCodexActiveAccount returns the current durable active-account pointer.
func (s *Service) CurrentCodexActiveAccount() domain.CodexActiveAccount {
	if s.codexAccounts == nil {
		return domain.CodexActiveAccount{}
	}
	s.codexAccounts.mu.Lock()
	defer s.codexAccounts.mu.Unlock()
	return s.codexAccounts.active
}

// CodexAccountLoginInProgress reports whether native login owns its mutation gate.
func (s *Service) CodexAccountLoginInProgress() bool {
	if s.codexAccounts == nil {
		return false
	}
	s.codexAccounts.mu.Lock()
	defer s.codexAccounts.mu.Unlock()
	return s.codexAccounts.login != nil && !terminalLoginStatus(s.codexAccounts.login.snapshot.Status)
}

// VerifyCodexAccountForSwitch strictly verifies an inactive switch target.
func (s *Service) VerifyCodexAccountForSwitch(ctx context.Context, accountID string) error {
	if s.codexAccounts == nil || s.codexAccounts.factory == nil {
		return apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex account management is unavailable")
	}
	if err := s.WaitCodexAccountBootstrap(ctx); err != nil {
		return err
	}
	if s.CodexAccountLoginInProgress() {
		return apierr.Conflict("CODEX_ACCOUNT_LOGIN_IN_PROGRESS", "Finish or close the Codex account login before switching accounts", nil)
	}
	capabilities := s.codexAccounts.detectCapabilities(ctx)
	switch capabilities.GlobalSwitch.State {
	case domain.CodexCapabilityUnsupported:
		return apierr.NotImplemented("CODEX_GLOBAL_CREDENTIAL_STORE_UNSUPPORTED", "Device-global Codex account switching requires file-backed credentials")
	case domain.CodexCapabilityUnknown:
		return apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex account switching capability could not be verified")
	}
	record, ok := s.codexAccounts.catalog.record(strings.TrimSpace(accountID))
	if !ok || record.Snapshot.Status != domain.CodexAccountStatusValid {
		return apierr.NotFound("CODEX_ACCOUNT_NOT_FOUND", "Codex account not found")
	}
	verifyCtx, cancel := context.WithTimeout(ctx, codexAccountAuthTimeout)
	defer cancel()
	client, err := s.codexAccounts.factory.Open(verifyCtx, ports.CodexAccountContext{Home: record.Home, Managed: true})
	if err != nil {
		return apierr.Conflict("CODEX_ACCOUNT_AUTH_UNVERIFIED", "Codex could not verify this account", nil)
	}
	observation, err := client.Read(verifyCtx, true)
	_ = client.Close()
	if err != nil || (observation.Authentication != domain.AgentAuthenticationAuthorized && observation.Authentication != domain.AgentAuthenticationNotApplicable) || !codexObservationMatchesAccount(record.Snapshot, observation) {
		return apierr.Conflict("CODEX_ACCOUNT_AUTH_UNVERIFIED", "Codex could not verify this account", nil)
	}
	return nil
}

// VerifyCurrentCodexAccount confirms that the normal device-global Codex home
// still represents the expected active AO account.
func (s *Service) VerifyCurrentCodexAccount(ctx context.Context, accountID string) error {
	if s.codexAccounts == nil || s.codexAccounts.factory == nil {
		return apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex account management is unavailable")
	}
	accountID = strings.TrimSpace(accountID)
	if s.CurrentCodexActiveAccount().AccountID != accountID {
		return apierr.Conflict("CODEX_GLOBAL_ACCOUNT_CHANGED", "The device Codex account changed", nil)
	}
	record, ok := s.codexAccounts.catalog.record(accountID)
	if !ok || record.Snapshot.Status != domain.CodexAccountStatusValid {
		return apierr.NotFound("CODEX_ACCOUNT_NOT_FOUND", "Codex account not found")
	}
	if err := s.codexAccounts.validateGlobalCredentialStore(); err != nil {
		return apierr.NotImplemented("CODEX_GLOBAL_CREDENTIAL_STORE_UNSUPPORTED", "Device-global Codex account switching requires file-backed credentials")
	}
	verifyCtx, cancel := context.WithTimeout(ctx, codexAccountAuthTimeout)
	defer cancel()
	client, err := s.codexAccounts.factory.Open(verifyCtx, ports.CodexAccountContext{Home: s.codexAccounts.globalHome, Managed: false})
	if err != nil {
		return apierr.Conflict("CODEX_GLOBAL_ACCOUNT_CHANGED", "The device Codex account could not be confirmed", nil)
	}
	observation, readErr := client.Read(verifyCtx, true)
	_ = client.Close()
	if readErr != nil || (observation.Authentication != domain.AgentAuthenticationAuthorized && observation.Authentication != domain.AgentAuthenticationNotApplicable) ||
		!codexObservationMatchesAccount(record.Snapshot, observation) {
		return apierr.Conflict("CODEX_GLOBAL_ACCOUNT_CHANGED", "The device Codex account changed", nil)
	}
	if refreshed, refreshErr := readOpaqueCredential(s.codexAccounts.globalCredentialPath()); refreshErr == nil {
		_ = writePrivateFileAtomic(filepath.Join(record.Home, codexCredentialFilename), refreshed)
	}
	return nil
}

// CheckpointAndActivateCodexAccount journals and verifies a credential activation.
func (s *Service) CheckpointAndActivateCodexAccount(ctx context.Context, switchID, targetID string, expectedRevision int64) (domain.CodexActiveAccount, error) {
	if s.codexAccounts == nil {
		return domain.CodexActiveAccount{}, apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex account management is unavailable")
	}
	if !isCanonicalUUIDv4(strings.TrimSpace(switchID)) {
		return domain.CodexActiveAccount{}, apierr.Invalid("INVALID_CODEX_ACCOUNT_ID", "Invalid Codex account switch identifier", nil)
	}
	current := s.CurrentCodexActiveAccount()
	if current.Revision != expectedRevision {
		return domain.CodexActiveAccount{}, apierr.Conflict("CODEX_ACCOUNT_REVISION_CONFLICT", "The active Codex account changed", nil)
	}
	stagingDir := filepath.Join(s.codexAccounts.switchStagingRoot, switchID)
	if err := ensurePrivateDirectory(stagingDir); err != nil {
		return domain.CodexActiveAccount{}, apierr.Unavailable("CODEX_ACCOUNT_SWITCH_ACTIVATION_UNCONFIRMED", "The Codex credential switch could not be staged")
	}
	defer func() { _ = os.RemoveAll(stagingDir) }()
	if err := s.codexAccounts.validateGlobalCredentialStore(); err != nil {
		return domain.CodexActiveAccount{}, apierr.NotImplemented("CODEX_GLOBAL_CREDENTIAL_STORE_UNSUPPORTED", "Device-global Codex account switching requires file-backed credentials")
	}
	globalCredential, err := readOpaqueCredential(s.codexAccounts.globalCredentialPath())
	if err != nil {
		return domain.CodexActiveAccount{}, apierr.NotImplemented("CODEX_GLOBAL_CREDENTIAL_STORE_UNSUPPORTED", "Device-global Codex account switching requires file-backed credentials")
	}
	if current.AccountID != "" {
		record, ok := s.codexAccounts.catalog.record(current.AccountID)
		if !ok {
			return domain.CodexActiveAccount{}, apierr.Conflict("CODEX_ACCOUNT_SWITCH_RECOVERY_REQUIRED", "The active Codex account slot is unavailable", nil)
		}
		verifyCtx, cancel := context.WithTimeout(ctx, codexAccountAuthTimeout)
		client, openErr := s.codexAccounts.factory.Open(verifyCtx, ports.CodexAccountContext{Home: s.codexAccounts.globalHome, Managed: false})
		if openErr != nil {
			cancel()
			return domain.CodexActiveAccount{}, apierr.Conflict("CODEX_GLOBAL_ACCOUNT_CHANGED", "The device Codex account could not be confirmed", nil)
		}
		observation, readErr := client.Read(verifyCtx, true)
		_ = client.Close()
		cancel()
		if readErr != nil || !codexObservationMatchesAccount(record.Snapshot, observation) {
			return domain.CodexActiveAccount{}, apierr.Conflict("CODEX_GLOBAL_ACCOUNT_CHANGED", "The device Codex account changed before switching", nil)
		}
		latestGlobal, latestErr := readOpaqueCredential(s.codexAccounts.globalCredentialPath())
		if latestErr != nil {
			return domain.CodexActiveAccount{}, apierr.Conflict("CODEX_GLOBAL_ACCOUNT_CHANGED", "The device Codex account changed before switching", nil)
		}
		globalCredential = latestGlobal
		checkpoint := filepath.Join(stagingDir, "source-auth.json")
		if err := writePrivateFileAtomic(checkpoint, globalCredential); err != nil {
			return domain.CodexActiveAccount{}, apierr.Unavailable("CODEX_ACCOUNT_SWITCH_ACTIVATION_UNCONFIRMED", "The active Codex credential could not be checkpointed")
		}
		if err := copyOpaqueCredential(checkpoint, filepath.Join(record.Home, codexCredentialFilename)); err != nil {
			return domain.CodexActiveAccount{}, apierr.Unavailable("CODEX_ACCOUNT_SWITCH_ACTIVATION_UNCONFIRMED", "The active Codex credential could not be checkpointed")
		}
	}
	target, ok := s.codexAccounts.catalog.record(strings.TrimSpace(targetID))
	if !ok {
		return domain.CodexActiveAccount{}, apierr.NotFound("CODEX_ACCOUNT_NOT_FOUND", "Codex account not found")
	}
	if err := copyOpaqueCredential(filepath.Join(target.Home, codexCredentialFilename), filepath.Join(stagingDir, "target-auth.json")); err != nil {
		return domain.CodexActiveAccount{}, apierr.Unavailable("CODEX_ACCOUNT_SWITCH_ACTIVATION_UNCONFIRMED", "The selected Codex credential could not be staged")
	}
	active, err := s.codexAccounts.activateFromCredential(ctx, strings.TrimSpace(targetID), expectedRevision, filepath.Join(stagingDir, "target-auth.json"), globalCredential)
	if err == nil {
		s.readiness.Invalidate(string(domain.HarnessCodex), readinessInvalidateAuthentication)
	}
	return active, err
}

// RestoreCodexAccountCredential restores and verifies the recorded source
// account only when the global credential still exactly matches the recorded
// source or target slot. Any other bytes may belong to an external login and
// are never overwritten by recovery.
func (s *Service) RestoreCodexAccountCredential(ctx context.Context, sourceAccountID, targetAccountID string) error {
	if s.codexAccounts == nil {
		return apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex account management is unavailable")
	}
	source, ok := s.codexAccounts.catalog.record(strings.TrimSpace(sourceAccountID))
	if !ok || source.Snapshot.Status != domain.CodexAccountStatusValid {
		return apierr.NotFound("CODEX_ACCOUNT_NOT_FOUND", "Codex account not found")
	}
	target, ok := s.codexAccounts.catalog.record(strings.TrimSpace(targetAccountID))
	if !ok || target.Snapshot.Status != domain.CodexAccountStatusValid {
		return apierr.NotFound("CODEX_ACCOUNT_NOT_FOUND", "Codex account not found")
	}
	sourceCredential, sourceErr := readOpaqueCredential(filepath.Join(source.Home, codexCredentialFilename))
	targetCredential, targetErr := readOpaqueCredential(filepath.Join(target.Home, codexCredentialFilename))
	globalPath := s.codexAccounts.globalCredentialPath()
	globalCredential, globalErr := readOpaqueCredential(globalPath)
	if sourceErr != nil || targetErr != nil || globalErr != nil {
		return apierr.Unavailable("CODEX_ACCOUNT_SWITCH_ACTIVATION_UNCONFIRMED", "The previous Codex credential could not be restored")
	}
	if !bytes.Equal(globalCredential, sourceCredential) {
		if !bytes.Equal(globalCredential, targetCredential) {
			return ports.ErrCodexGlobalAccountChanged
		}
		latestGlobal, latestErr := readOpaqueCredential(globalPath)
		if latestErr != nil || !bytes.Equal(latestGlobal, targetCredential) {
			return ports.ErrCodexGlobalAccountChanged
		}
		if err := writeGlobalCredentialAtomic(globalPath, sourceCredential); err != nil {
			return apierr.Unavailable("CODEX_ACCOUNT_SWITCH_ACTIVATION_UNCONFIRMED", "The previous Codex credential could not be restored")
		}
	}
	verifyCtx, cancel := context.WithTimeout(ctx, codexAccountAuthTimeout)
	defer cancel()
	client, err := s.codexAccounts.factory.Open(verifyCtx, ports.CodexAccountContext{Home: s.codexAccounts.globalHome, Managed: false})
	if err != nil {
		return apierr.Unavailable("CODEX_ACCOUNT_SWITCH_ACTIVATION_UNCONFIRMED", "The previous Codex account could not be verified")
	}
	observation, readErr := client.Read(verifyCtx, true)
	_ = client.Close()
	if readErr != nil ||
		(observation.Authentication != domain.AgentAuthenticationAuthorized && observation.Authentication != domain.AgentAuthenticationNotApplicable) ||
		!codexObservationMatchesAccount(source.Snapshot, observation) {
		return apierr.Unavailable("CODEX_ACCOUNT_SWITCH_ACTIVATION_UNCONFIRMED", "The previous Codex account could not be verified")
	}
	if refreshed, refreshErr := readOpaqueCredential(s.codexAccounts.globalCredentialPath()); refreshErr == nil {
		_ = writePrivateFileAtomic(filepath.Join(source.Home, codexCredentialFilename), refreshed)
	}
	s.readiness.Invalidate(string(domain.HarnessCodex), readinessInvalidateAuthentication)
	return nil
}

var _ ports.CodexAccountCredentialManager = (*Service)(nil)

// SetCodexAccountSwitchCoordinator wires the daemon-owned global switch coordinator.
func (s *Service) SetCodexAccountSwitchCoordinator(coordinator CodexAccountSwitchCoordinator) {
	s.codexSwitches = coordinator
}

// StartCodexAccountSwitch delegates an accepted switch to Session Manager.
func (s *Service) StartCodexAccountSwitch(ctx context.Context, cfg ports.CodexAccountSwitchConfig) (domain.CodexAccountSwitch, error) {
	if s.codexSwitches == nil {
		return domain.CodexAccountSwitch{}, apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex account switching is unavailable")
	}
	return s.codexSwitches.StartCodexAccountSwitch(ctx, cfg)
}

// GetCodexAccountSwitch reads one durable switch operation.
func (s *Service) GetCodexAccountSwitch(ctx context.Context, id string) (domain.CodexAccountSwitch, error) {
	if s.codexSwitches == nil {
		return domain.CodexAccountSwitch{}, apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex account switching is unavailable")
	}
	return s.codexSwitches.GetCodexAccountSwitch(ctx, id)
}

// CancelCodexAccountSwitch requests cancellation before the stop boundary.
func (s *Service) CancelCodexAccountSwitch(ctx context.Context, id string) (domain.CodexAccountSwitch, error) {
	if s.codexSwitches == nil {
		return domain.CodexAccountSwitch{}, apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex account switching is unavailable")
	}
	return s.codexSwitches.CancelCodexAccountSwitch(ctx, id)
}

// RecoverCodexAccountSwitch retries recorded incomplete work only.
func (s *Service) RecoverCodexAccountSwitch(ctx context.Context, id string) (domain.CodexAccountSwitch, error) {
	if s.codexSwitches == nil {
		return domain.CodexAccountSwitch{}, apierr.Unavailable("CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE", "Codex account switching is unavailable")
	}
	return s.codexSwitches.RecoverCodexAccountSwitch(ctx, id)
}
