package ports

import (
	"context"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

var (
	// ErrCodexAccountSwitchInProgress means the global Codex mutation gate is held.
	ErrCodexAccountSwitchInProgress = errors.New("codex account switch already in progress")
	// ErrCodexAccountAlreadyActive rejects selecting the current account.
	ErrCodexAccountAlreadyActive = errors.New("codex account is already active")
	// ErrCodexAccountSwitchNotFound means the durable operation does not exist.
	ErrCodexAccountSwitchNotFound = errors.New("codex account switch not found")
	// ErrCodexAccountSwitchCancellationUnsafe marks the durable stop boundary.
	ErrCodexAccountSwitchCancellationUnsafe = errors.New("codex account switch can no longer be cancelled")
	// ErrCodexAccountRevisionConflict reports a stale active-account revision.
	ErrCodexAccountRevisionConflict = errors.New("codex account revision conflict")
	// ErrCodexAccountSwitchIdempotencyConflict rejects reused mismatched keys.
	ErrCodexAccountSwitchIdempotencyConflict = errors.New("codex account switch idempotency conflict")
	// ErrCodexAccountLoginInProgress means native login owns the account gate.
	ErrCodexAccountLoginInProgress = errors.New("codex account login in progress")
	// ErrCodexGlobalAccountChanged reports an external account change during admission.
	ErrCodexGlobalAccountChanged = errors.New("global codex account changed")
	// ErrCodexGlobalCredentialStoreUnsupported rejects non-file-backed switching.
	ErrCodexGlobalCredentialStoreUnsupported = errors.New("global codex credential store is not safely file-backed")
	// ErrCodexRunningSessionNotResumable rejects switching before stopping a controller without exact native identity.
	ErrCodexRunningSessionNotResumable = errors.New("running codex session cannot be resumed exactly")
)

// CodexAccountCredentialManager is consumed by Session Manager's global switch
// coordinator. It exposes account identities and atomic credential activation,
// never credential bytes or homes.
type CodexAccountCredentialManager interface {
	WaitCodexAccountBootstrap(context.Context) error
	CurrentCodexActiveAccount() domain.CodexActiveAccount
	CodexAccountLoginInProgress() bool
	VerifyCodexAccountForSwitch(context.Context, string) error
	VerifyCurrentCodexAccount(context.Context, string) error
	CheckpointAndActivateCodexAccount(context.Context, string, string, int64) (domain.CodexActiveAccount, error)
	RestoreCodexAccountCredential(context.Context, string, string) error
}

// CodexAccountSwitchConfig is the validated input to the global switch coordinator.
type CodexAccountSwitchConfig struct {
	TargetAccountID         string
	ExpectedAccountRevision int64
	IdempotencyKey          string
}

// CodexAccountSwitchStore persists global switch facts and CAS transitions.
type CodexAccountSwitchStore interface {
	CreateCodexAccountSwitch(context.Context, domain.CodexAccountSwitch) (domain.CodexAccountSwitch, bool, error)
	GetCodexAccountSwitch(context.Context, string) (domain.CodexAccountSwitch, bool, error)
	GetCodexAccountSwitchByIdempotency(context.Context, string) (domain.CodexAccountSwitch, bool, error)
	GetActiveCodexAccountSwitch(context.Context) (domain.CodexAccountSwitch, bool, error)
	UpdateCodexAccountSwitch(context.Context, domain.CodexAccountSwitch, domain.CodexAccountSwitchPhase) (bool, error)
	InsertCodexAccountSwitchSession(context.Context, string, domain.CodexAccountSwitchSession) error
	ListCodexAccountSwitchSessions(context.Context, string) ([]domain.CodexAccountSwitchSession, error)
	UpdateCodexAccountSwitchSession(context.Context, string, domain.CodexAccountSwitchSession, string, string) (bool, error)
}
