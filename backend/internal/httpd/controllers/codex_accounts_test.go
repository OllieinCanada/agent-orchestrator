package controllers_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	agentsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/agent"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/shellterm"
)

type fakeCodexAccounts struct {
	result             agentsvc.CodexAccounts
	ensureIDs          []string
	includeUsage       bool
	events             chan agentsvc.CodexAccounts
	loginStart         agentsvc.CodexAccountLoginTerminalStart
	verifiedOperation  string
	cancelledOperation string
	switchConfig       ports.CodexAccountSwitchConfig
	switchResult       domain.CodexAccountSwitch
	switchErr          error
}

func (f *fakeCodexAccounts) CachedCodexAccounts(context.Context) (agentsvc.CodexAccounts, error) {
	return f.result, nil
}
func (f *fakeCodexAccounts) EnsureCodexAccounts(_ context.Context, ids []string, includeUsage bool) (agentsvc.CodexAccounts, error) {
	f.ensureIDs, f.includeUsage = ids, includeUsage
	return f.result, nil
}
func (f *fakeCodexAccounts) SubscribeCodexAccounts(ctx context.Context) (<-chan agentsvc.CodexAccounts, error) {
	if f.events != nil {
		return f.events, nil
	}
	ch := make(chan agentsvc.CodexAccounts)
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}
func (f *fakeCodexAccounts) OpenCodexAccountLoginTerminal(context.Context) (agentsvc.CodexAccountLoginTerminalStart, error) {
	return f.loginStart, nil
}
func (f *fakeCodexAccounts) VerifyCodexAccountLogin(_ context.Context, id string) (domain.CodexAccountLoginOperation, error) {
	f.verifiedOperation = id
	return domain.CodexAccountLoginOperation{OperationID: id, Status: domain.CodexAccountLoginUnverified, ReasonCode: domain.CodexAccountLoginReasonUnverified, Reason: "unverified"}, nil
}
func (f *fakeCodexAccounts) CancelCodexAccountLogin(_ context.Context, id string) (domain.CodexAccountLoginOperation, error) {
	f.cancelledOperation = id
	return domain.CodexAccountLoginOperation{OperationID: id, Status: domain.CodexAccountLoginCancelled, ReasonCode: domain.CodexAccountLoginReasonCancelled, Reason: "cancelled"}, nil
}
func (f *fakeCodexAccounts) StartCodexAccountSwitch(_ context.Context, cfg ports.CodexAccountSwitchConfig) (domain.CodexAccountSwitch, error) {
	f.switchConfig = cfg
	return f.switchResult, f.switchErr
}
func (f *fakeCodexAccounts) GetCodexAccountSwitch(context.Context, string) (domain.CodexAccountSwitch, error) {
	return f.switchResult, nil
}
func (f *fakeCodexAccounts) CancelCodexAccountSwitch(context.Context, string) (domain.CodexAccountSwitch, error) {
	return f.switchResult, nil
}
func (f *fakeCodexAccounts) RecoverCodexAccountSwitch(context.Context, string) (domain.CodexAccountSwitch, error) {
	return f.switchResult, nil
}

func codexAccountsFixture() agentsvc.CodexAccounts {
	supported := domain.CodexCapabilityObservation{State: domain.CodexCapabilitySupported, ReasonCode: "supported", Reason: "available"}
	remaining := 95.0
	used := 5.0
	return agentsvc.CodexAccounts{
		ActiveAccountID: "72d4db6e-da2c-414c-a6a9-fdbd09a006b6",
		AccountRevision: 3,
		Accounts: []domain.CodexAccountSnapshot{{
			ID: "72d4db6e-da2c-414c-a6a9-fdbd09a006b6", Label: "person@example.com", Source: domain.CodexAccountSourceManaged,
			Status: domain.CodexAccountStatusValid, ReasonCode: domain.CodexAccountReasonValid, Reason: "available", Active: true,
			Authentication: domain.AgentAuthenticationObservation{State: domain.AgentAuthenticationAuthorized, Freshness: domain.AgentReadinessFresh, ReasonCode: domain.AgentReadinessReasonAuthorized, Reason: "signed in"},
			AuthMethod:     domain.CodexAuthMethodChatGPT,
			Capacity:       domain.CodexCapacitySnapshot{State: domain.CodexCapacityAvailable, Freshness: domain.AgentReadinessFresh, UsedPercent: &used, RemainingPercent: &remaining, ReasonCode: domain.CodexCapacityReasonAvailable, Reason: "available", AdditionalBuckets: []domain.CodexCapacityBucket{}},
		}},
		Capabilities: domain.CodexAccountCapabilities{AccountRead: supported, NativeLogin: supported, CapacityRead: supported, UsageRead: supported, ThreadResume: supported, AccountManagement: supported, GlobalSwitch: supported},
	}
}

func newCodexAccountServer(t *testing.T, fake *fakeCodexAccounts) *httptest.Server {
	t.Helper()
	return httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, slog.New(slog.DiscardHandler), nil, httpd.APIDeps{CodexAccounts: fake}, httpd.ControlDeps{}))
}

func TestCodexAccountRoutesExposeSafeCachedAndEnsureShapes(t *testing.T) {
	fake := &fakeCodexAccounts{result: codexAccountsFixture()}
	srv := newCodexAccountServer(t, fake)
	defer srv.Close()
	body, status, _ := doRequest(t, srv, http.MethodGet, "/api/v1/agents/codex/accounts", "")
	text := string(body)
	if status != http.StatusOK || !strings.Contains(text, `"activeAccountId"`) || !strings.Contains(text, `"remainingPercent":95`) {
		t.Fatalf("GET status=%d body=%s", status, body)
	}
	for _, forbidden := range []string{"auth.json", "credential-home", "codexHome", "nativeSessionId", "generationId", "idempotencyKey"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("GET leaked %q: %s", forbidden, body)
		}
	}
	body, status, _ = doRequest(t, srv, http.MethodPost, "/api/v1/agents/codex/accounts/ensure", `{"accountIds":["a","a"],"includeUsage":true}`)
	if status != http.StatusOK || len(fake.ensureIDs) != 2 || !fake.includeUsage {
		t.Fatalf("ensure status=%d ids=%#v includeUsage=%v body=%s", status, fake.ensureIDs, fake.includeUsage, body)
	}
	body, status, _ = doRequest(t, srv, http.MethodPost, "/api/v1/agents/codex/accounts/ensure", `{"accountIds":[],"force":true}`)
	if status != http.StatusBadRequest || !strings.Contains(string(body), `"code":"INVALID_JSON"`) {
		t.Fatalf("strict ensure status=%d body=%s", status, body)
	}
}

func TestCodexAccountLoginTerminalAndVerificationRoutesExposeNoCommandOrPath(t *testing.T) {
	fake := &fakeCodexAccounts{result: codexAccountsFixture(), loginStart: agentsvc.CodexAccountLoginTerminalStart{
		Operation:     domain.CodexAccountLoginOperation{OperationID: "op-1", Status: domain.CodexAccountLoginPending, ReasonCode: domain.CodexAccountLoginReasonPending, Reason: "waiting", ExpiresAt: time.Now().Add(time.Minute)},
		ShellTerminal: shellterm.ShellTerminal{HandleID: "shellterm-login-1", WorkingDir: "/private/secret", Title: "Add Codex account", CreatedAt: time.Now()},
	}}
	srv := newCodexAccountServer(t, fake)
	defer srv.Close()
	body, status, _ := doRequest(t, srv, http.MethodPost, "/api/v1/agents/codex/accounts/login-terminal", "")
	text := string(body)
	if status != http.StatusAccepted || !strings.Contains(text, `"operationId":"op-1"`) || !strings.Contains(text, `"handleId":"shellterm-login-1"`) {
		t.Fatalf("login status=%d body=%s", status, body)
	}
	for _, forbidden := range []string{"workingDir", "/private/secret", "argv", "CODEX_HOME"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("login response leaked %q: %s", forbidden, body)
		}
	}
	body, status, _ = doRequest(t, srv, http.MethodPost, "/api/v1/agents/codex/accounts/login-terminal", `{}`)
	if status != http.StatusBadRequest || !strings.Contains(string(body), `"code":"INVALID_REQUEST_BODY"`) {
		t.Fatalf("body rejection status=%d body=%s", status, body)
	}
	_, status, _ = doRequest(t, srv, http.MethodPost, "/api/v1/agents/codex/accounts/login-operations/op-1/verify", "")
	if status != http.StatusOK || fake.verifiedOperation != "op-1" {
		t.Fatalf("verify status=%d id=%q", status, fake.verifiedOperation)
	}
}

func TestCodexAccountSwitchRequiresIdempotencyAndRedactsPrivateIdentity(t *testing.T) {
	fake := &fakeCodexAccounts{result: codexAccountsFixture(), switchResult: domain.CodexAccountSwitch{
		ID: "switch-1", SourceAccountID: "source", TargetAccountID: "target", Phase: domain.CodexAccountSwitchRequested,
		Reason: "preparing", Sessions: []domain.CodexAccountSwitchSession{{SessionID: "ao-1", InterfaceMode: domain.SessionModeChat, WasRunning: true, StopState: "pending", RestartState: "pending", NativeSessionID: "native-secret", SourceGeneration: "generation-secret"}},
		IdempotencyKey: "private-key", RequestFingerprint: "private-fingerprint", ExpectedAccountRevision: 3,
	}}
	srv := newCodexAccountServer(t, fake)
	defer srv.Close()
	body, status, _ := doRequest(t, srv, http.MethodPost, "/api/v1/agents/codex/account-switches", `{"targetAccountId":"target","expectedAccountRevision":3,"idempotencyKey":""}`)
	if status != http.StatusBadRequest || !strings.Contains(string(body), `"code":"IDEMPOTENCY_KEY_REQUIRED"`) {
		t.Fatalf("missing key status=%d body=%s", status, body)
	}
	body, status, _ = doRequest(t, srv, http.MethodPost, "/api/v1/agents/codex/account-switches", `{"targetAccountId":"target","expectedAccountRevision":3,"idempotencyKey":"request-key"}`)
	text := string(body)
	if status != http.StatusAccepted || fake.switchConfig.IdempotencyKey != "request-key" || !strings.Contains(text, `"sessionId":"ao-1"`) {
		t.Fatalf("switch status=%d config=%#v body=%s", status, fake.switchConfig, body)
	}
	for _, forbidden := range []string{"native-secret", "generation-secret", "private-key", "private-fingerprint"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("switch leaked %q: %s", forbidden, body)
		}
	}
}

func TestCodexAccountSwitchWithoutReconciledSourceReturnsTypedConflict(t *testing.T) {
	fake := &fakeCodexAccounts{
		result:    codexAccountsFixture(),
		switchErr: ports.ErrCodexActiveAccountUnavailable,
	}
	srv := newCodexAccountServer(t, fake)
	defer srv.Close()
	body, status, _ := doRequest(t, srv, http.MethodPost, "/api/v1/agents/codex/account-switches", `{"targetAccountId":"target","expectedAccountRevision":3,"idempotencyKey":"request-key"}`)
	if status != http.StatusConflict || !strings.Contains(string(body), `"code":"CODEX_ACCOUNT_AUTH_UNVERIFIED"`) {
		t.Fatalf("unreconciled switch status=%d body=%s", status, body)
	}
}

func TestCodexAccountEventStreamSendsNamedCachedState(t *testing.T) {
	events := make(chan agentsvc.CodexAccounts, 1)
	events <- codexAccountsFixture()
	close(events)
	fake := &fakeCodexAccounts{result: codexAccountsFixture(), events: events}
	srv := newCodexAccountServer(t, fake)
	defer srv.Close()
	response, err := http.Get(srv.URL + "/api/v1/agents/codex/accounts/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "event: codex_account") || !strings.Contains(string(body), `"accountRevision":3`) {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
}
