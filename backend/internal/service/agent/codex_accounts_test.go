package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/shellterm"
)

type fakeCodexAccountFactory struct {
	mu               sync.Mutex
	opens            int
	capabilityChecks int
	capabilities     domain.CodexAccountCapabilities
	open             func(ports.CodexAccountContext) (ports.CodexAccountClient, error)
}

func (f *fakeCodexAccountFactory) Open(_ context.Context, account ports.CodexAccountContext) (ports.CodexAccountClient, error) {
	f.mu.Lock()
	f.opens++
	open := f.open
	f.mu.Unlock()
	if open == nil {
		return nil, errors.New("unexpected account client open")
	}
	return open(account)
}

func (f *fakeCodexAccountFactory) Capabilities(context.Context) domain.CodexAccountCapabilities {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.capabilityChecks++
	return f.capabilities
}

type fakeCodexAccountClient struct {
	read            ports.CodexAccountObservation
	readErr         error
	readFn          func(context.Context, bool) (ports.CodexAccountObservation, error)
	readStarted     chan struct{}
	readRelease     chan struct{}
	capacity        ports.CodexCapacityObservation
	capacityErr     error
	capacityStarted chan struct{}
	capacityRelease chan struct{}
	usage           ports.CodexUsageObservation
	resetOutcome    domain.CodexResetCreditOutcome
	resetErr        error
	resetKeys       []string
	resetFn         func(string) (domain.CodexResetCreditOutcome, error)
	events          chan ports.CodexAccountEvent
}

func (c *fakeCodexAccountClient) Read(ctx context.Context, refreshToken bool) (ports.CodexAccountObservation, error) {
	if c.readFn != nil {
		return c.readFn(ctx, refreshToken)
	}
	if c.readStarted != nil {
		select {
		case c.readStarted <- struct{}{}:
		default:
		}
	}
	if c.readRelease != nil {
		select {
		case <-c.readRelease:
		case <-ctx.Done():
			return ports.CodexAccountObservation{}, ctx.Err()
		}
	}
	return c.read, c.readErr
}

func (c *fakeCodexAccountClient) ReadCapacity(ctx context.Context) (ports.CodexCapacityObservation, error) {
	if c.capacityStarted != nil {
		select {
		case c.capacityStarted <- struct{}{}:
		default:
		}
	}
	if c.capacityRelease != nil {
		select {
		case <-c.capacityRelease:
		case <-ctx.Done():
			return ports.CodexCapacityObservation{}, ctx.Err()
		}
	}
	return c.capacity, c.capacityErr
}

func (c *fakeCodexAccountClient) ReadUsage(context.Context) (ports.CodexUsageObservation, error) {
	return c.usage, nil
}
func (c *fakeCodexAccountClient) ConsumeResetCredit(_ context.Context, idempotencyKey string) (domain.CodexResetCreditOutcome, error) {
	c.resetKeys = append(c.resetKeys, idempotencyKey)
	if c.resetFn != nil {
		return c.resetFn(idempotencyKey)
	}
	return c.resetOutcome, c.resetErr
}
func (c *fakeCodexAccountClient) Events() <-chan ports.CodexAccountEvent {
	if c.events == nil {
		ch := make(chan ports.CodexAccountEvent)
		close(ch)
		return ch
	}
	return c.events
}
func (c *fakeCodexAccountClient) Close() error { return nil }

type fakeCodexAccountStateStore struct {
	mu     sync.Mutex
	active domain.CodexActiveAccount
	found  bool
}

func (s *fakeCodexAccountStateStore) GetCodexActiveAccount(context.Context) (domain.CodexActiveAccount, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, s.found, nil
}

func (s *fakeCodexAccountStateStore) SetCodexActiveAccount(_ context.Context, id string, expected int64, at time.Time) (domain.CodexActiveAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if (!s.found && expected != 0) || (s.found && s.active.Revision != expected) {
		return domain.CodexActiveAccount{}, ports.ErrCodexAccountRevisionConflict
	}
	s.active = domain.CodexActiveAccount{AccountID: id, Revision: expected + 1, ActivatedAt: at, UpdatedAt: at}
	s.found = true
	return s.active, nil
}

type fakeCodexLoginTerminal struct {
	mu              sync.Mutex
	opened          []shellterm.OpenCommandTerminalInput
	closed          []string
	result          shellterm.ShellTerminal
	closeErr        error
	writeCredential bool
}

func (f *fakeCodexLoginTerminal) OpenCommandTerminal(_ context.Context, in shellterm.OpenCommandTerminalInput) (shellterm.ShellTerminal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, in)
	if f.writeCredential {
		if err := writePrivateFileAtomic(filepath.Join(in.Env["CODEX_HOME"], codexCredentialFilename), []byte("opaque-login-credential")); err != nil {
			return shellterm.ShellTerminal{}, err
		}
	}
	return f.result, nil
}

func (f *fakeCodexLoginTerminal) CloseShellTerminal(_ context.Context, handle string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closeErr != nil {
		return f.closeErr
	}
	f.closed = append(f.closed, handle)
	return nil
}

func supportedCodexAccountCapabilities() domain.CodexAccountCapabilities {
	supported := domain.CodexCapabilityObservation{State: domain.CodexCapabilitySupported, ReasonCode: domain.CodexCapabilityReasonSupported, Reason: "supported"}
	return domain.CodexAccountCapabilities{
		AccountRead: supported, NativeLogin: supported, CapacityRead: supported,
		UsageRead: supported, ResetCreditConsume: supported, ThreadResume: supported, AccountManagement: supported, GlobalSwitch: supported,
	}
}

func newTestCodexAccountManager(t *testing.T, factory ports.CodexAccountClientFactory, state CodexAccountStateStore) *codexAccountManager {
	t.Helper()
	root := t.TempDir()
	return newCodexAccountManager(context.Background(),
		filepath.Join(root, "accounts"), filepath.Join(root, "pending-accounts"),
		filepath.Join(root, "switch-staging"), filepath.Join(root, "device-home"),
		factory, state, nil)
}

func TestCachedCodexAccountsPerformsNoFilesystemOrNativeWork(t *testing.T) {
	factory := &fakeCodexAccountFactory{open: func(ports.CodexAccountContext) (ports.CodexAccountClient, error) {
		t.Fatal("cached account read opened Codex")
		return nil, nil
	}}
	manager := newTestCodexAccountManager(t, factory, nil)
	result := manager.cached()
	if len(result.Accounts) != 0 || result.AccountRevision != 0 {
		t.Fatalf("cached accounts = %#v", result)
	}
	if factory.opens != 0 || factory.capabilityChecks != 0 {
		t.Fatalf("native work: opens=%d capability=%d", factory.opens, factory.capabilityChecks)
	}
}

func TestNativeLoginTerminalUsesOnePrivatePendingHomeAndNoName(t *testing.T) {
	manager := newTestCodexAccountManager(t, nil, nil)
	manager.newID = func() string { return "b60a377d-da68-4a61-86f2-f31f04c571f2" }
	manager.executable = func() (string, error) { return "/Applications/AO.app/Contents/MacOS/ao", nil }
	terminal := &fakeCodexLoginTerminal{result: shellterm.ShellTerminal{HandleID: "shellterm-login-1", Title: "Add Codex account"}}
	manager.terminal = terminal
	started, err := manager.openLoginTerminal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if started.Operation.Status != domain.CodexAccountLoginPending || started.Operation.OperationID == "" {
		t.Fatalf("login start = %#v", started)
	}
	if len(terminal.opened) != 1 {
		t.Fatalf("terminal opens = %d", len(terminal.opened))
	}
	opened := terminal.opened[0]
	if !slices.Equal(opened.Argv, []string{"/Applications/AO.app/Contents/MacOS/ao", "codex-login"}) {
		t.Fatalf("argv = %#v", opened.Argv)
	}
	home := opened.Env["CODEX_HOME"]
	if home == "" || home != opened.WorkingDir || !pathWithin(manager.pendingRoot, home) {
		t.Fatalf("pending login home = %q, workdir = %q", home, opened.WorkingDir)
	}
}

func TestNativeLoginVerificationCreatesAndActivatesFirstAccount(t *testing.T) {
	email := "person@example.com"
	client := &fakeCodexAccountClient{read: ports.CodexAccountObservation{Authentication: domain.AgentAuthenticationAuthorized, Method: domain.CodexAuthMethodChatGPT, Email: &email}}
	factory := &fakeCodexAccountFactory{capabilities: supportedCodexAccountCapabilities(), open: func(ports.CodexAccountContext) (ports.CodexAccountClient, error) { return client, nil }}
	state := &fakeCodexAccountStateStore{}
	manager := newTestCodexAccountManager(t, factory, state)
	manager.globalAuth = accountAuthenticationObservation(time.Now().UTC(), domain.AgentAuthenticationUnauthorized)
	ids := []string{"b60a377d-da68-4a61-86f2-f31f04c571f2", testAccountID}
	index := 0
	manager.newID = func() string { id := ids[index]; index++; return id }
	manager.catalog.newID = func() string { return testAccountID }
	manager.executable = func() (string, error) { return "/ao", nil }
	terminal := &fakeCodexLoginTerminal{writeCredential: true, result: shellterm.ShellTerminal{HandleID: "shellterm-login-1", Title: "Add Codex account"}}
	manager.terminal = terminal
	started, err := manager.openLoginTerminal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	completed, err := manager.verifyLogin(context.Background(), started.Operation.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != domain.CodexAccountLoginCompleted || completed.Account == nil || !completed.Account.Active || completed.Account.Label != email {
		t.Fatalf("completed login = %#v", completed)
	}
	if active := manager.activeAccountID(); active != testAccountID {
		t.Fatalf("active account = %q", active)
	}
	if state.active.Revision != 1 || state.active.AccountID != testAccountID {
		t.Fatalf("durable active account = %#v", state.active)
	}
	if len(terminal.closed) != 1 || terminal.closed[0] != "shellterm-login-1" {
		t.Fatalf("closed terminals = %#v", terminal.closed)
	}
	credential := filepath.Join(manager.catalog.root, testAccountID, codexCredentialHomeDirectory, codexCredentialFilename)
	data, err := os.ReadFile(credential)
	if err != nil || string(data) != "opaque-login-credential" {
		t.Fatalf("opaque credential = %q, err=%v", data, err)
	}
}

func TestLoginCloseFailureRetainsPendingOperation(t *testing.T) {
	manager := newTestCodexAccountManager(t, nil, nil)
	manager.newID = func() string { return "b60a377d-da68-4a61-86f2-f31f04c571f2" }
	manager.executable = func() (string, error) { return "/ao", nil }
	terminal := &fakeCodexLoginTerminal{result: shellterm.ShellTerminal{HandleID: "shellterm-login-1"}, closeErr: errors.New("pty busy")}
	manager.terminal = terminal
	started, err := manager.openLoginTerminal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.cancelLogin(context.Background(), started.Operation.OperationID); err == nil {
		t.Fatal("cancel unexpectedly succeeded")
	}
	manager.mu.Lock()
	operation := manager.login.snapshot
	manager.mu.Unlock()
	if operation.Status != domain.CodexAccountLoginUnverified {
		t.Fatalf("operation after close failure = %#v", operation)
	}
}

func TestBootstrapImportsOpaqueDeviceCredentialWithoutMutatingDeviceHome(t *testing.T) {
	root := t.TempDir()
	device := filepath.Join(root, "device")
	if err := ensurePrivateDirectory(device); err != nil {
		t.Fatal(err)
	}
	deviceCredential := filepath.Join(device, codexCredentialFilename)
	original := []byte("opaque-device-auth\x00\xff")
	if err := writePrivateFileAtomic(deviceCredential, original); err != nil {
		t.Fatal(err)
	}
	email := "device@example.com"
	client := &fakeCodexAccountClient{read: ports.CodexAccountObservation{Authentication: domain.AgentAuthenticationAuthorized, Method: domain.CodexAuthMethodChatGPT, Email: &email}}
	factory := &fakeCodexAccountFactory{capabilities: supportedCodexAccountCapabilities(), open: func(ports.CodexAccountContext) (ports.CodexAccountClient, error) { return client, nil }}
	state := &fakeCodexAccountStateStore{}
	manager := newCodexAccountManager(context.Background(), filepath.Join(root, "accounts"), filepath.Join(root, "pending"), filepath.Join(root, "staging"), device, factory, state, nil)
	ids := []string{"b60a377d-da68-4a61-86f2-f31f04c571f2", testAccountID}
	index := 0
	manager.newID = func() string { id := ids[index]; index++; return id }
	manager.catalog.newID = func() string { return testAccountID }
	manager.bootstrap()
	if manager.bootstrapErr != nil {
		t.Fatal(manager.bootstrapErr)
	}
	if manager.activeAccountID() != testAccountID {
		t.Fatalf("active account = %q", manager.activeAccountID())
	}
	after, err := os.ReadFile(deviceCredential)
	if err != nil || !slices.Equal(after, original) {
		t.Fatalf("device credential changed: %q err=%v", after, err)
	}
	if _, err := os.Stat(filepath.Join(root, "runtime")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bootstrap created an obsolete private runtime: %v", err)
	}
}

func TestGlobalReconciliationKeepsMatchingDeviceAccountActiveWithoutProactiveRefresh(t *testing.T) {
	root := t.TempDir()
	globalHome := filepath.Join(root, "global-codex")
	if err := ensurePrivateDirectory(globalHome); err != nil {
		t.Fatal(err)
	}
	globalCredential := filepath.Join(globalHome, codexCredentialFilename)
	credential := []byte("opaque-codex-credential\x00\xff")
	if err := writePrivateFileAtomic(globalCredential, credential); err != nil {
		t.Fatal(err)
	}
	email := "device@example.com"
	observation := ports.CodexAccountObservation{
		Authentication: domain.AgentAuthenticationAuthorized,
		Method:         domain.CodexAuthMethodChatGPT,
		Email:          &email,
	}
	state := &fakeCodexAccountStateStore{
		active: domain.CodexActiveAccount{AccountID: testAccountID, Revision: 1},
		found:  true,
	}
	manager := newCodexAccountManager(
		context.Background(),
		filepath.Join(root, "accounts"),
		filepath.Join(root, "pending"),
		filepath.Join(root, "staging"),
		globalHome,
		nil,
		state,
		nil,
	)
	manager.catalog.newID = func() string { return testAccountID }
	record := commitTestAccount(t, manager.catalog, manager.pendingRoot, "b60a377d-da68-4a61-86f2-f31f04c571f2", observation)
	if err := writePrivateFileAtomic(filepath.Join(record.Home, codexCredentialFilename), credential); err != nil {
		t.Fatal(err)
	}
	var refreshRequests []bool
	manager.factory = &fakeCodexAccountFactory{
		capabilities: supportedCodexAccountCapabilities(),
		open: func(ports.CodexAccountContext) (ports.CodexAccountClient, error) {
			return &fakeCodexAccountClient{readFn: func(_ context.Context, refresh bool) (ports.CodexAccountObservation, error) {
				refreshRequests = append(refreshRequests, refresh)
				if refresh {
					return ports.CodexAccountObservation{}, errors.New("proactive refresh rejected for copied credential")
				}
				return observation, nil
			}}, nil
		},
	}

	if err := manager.bootstrapInner(); err != nil {
		t.Fatal(err)
	}
	view := manager.cached()
	if view.ActiveAccountID != testAccountID || view.UnmanagedGlobalAccount != nil || len(view.Accounts) != 1 || !view.Accounts[0].Active {
		t.Fatalf("reconciled device account = %#v, refresh requests = %#v", view, refreshRequests)
	}
	if slices.Contains(refreshRequests, true) {
		t.Fatalf("reconciliation requested proactive refresh: %#v", refreshRequests)
	}
}

func TestGlobalReconciliationAutoImportsExternalAccountChanges(t *testing.T) {
	root := t.TempDir()
	globalHome := filepath.Join(root, "global-codex")
	if err := ensurePrivateDirectory(globalHome); err != nil {
		t.Fatal(err)
	}
	globalCredential := filepath.Join(globalHome, codexCredentialFilename)
	if err := writePrivateFileAtomic(globalCredential, []byte("credential-a")); err != nil {
		t.Fatal(err)
	}
	emailA, emailB := "a@example.com", "b@example.com"
	observationForHome := func(home string) (ports.CodexAccountObservation, error) {
		credential, err := readOpaqueCredential(filepath.Join(home, codexCredentialFilename))
		if err != nil {
			return ports.CodexAccountObservation{}, err
		}
		switch string(credential) {
		case "credential-a":
			return ports.CodexAccountObservation{Authentication: domain.AgentAuthenticationAuthorized, Method: domain.CodexAuthMethodChatGPT, Email: &emailA}, nil
		case "credential-b":
			return ports.CodexAccountObservation{Authentication: domain.AgentAuthenticationAuthorized, Method: domain.CodexAuthMethodChatGPT, Email: &emailB}, nil
		default:
			return ports.CodexAccountObservation{Authentication: domain.AgentAuthenticationUnknown}, nil
		}
	}
	factory := &fakeCodexAccountFactory{capabilities: supportedCodexAccountCapabilities()}
	factory.open = func(account ports.CodexAccountContext) (ports.CodexAccountClient, error) {
		observation, err := observationForHome(account.Home)
		return &fakeCodexAccountClient{read: observation, readErr: err}, nil
	}
	state := &fakeCodexAccountStateStore{}
	manager := newCodexAccountManager(context.Background(), filepath.Join(root, "accounts"), filepath.Join(root, "pending"), filepath.Join(root, "staging"), globalHome, factory, state, nil)
	operationIDs := []string{
		"b60a377d-da68-4a61-86f2-f31f04c571f2", "a8600b36-0e78-461d-9dd8-378fe18e271d",
		"8b71ac75-4d81-45ee-bbe2-952cbe15e353", "82db3fd8-e87f-4c5c-8511-b63fde7937ae",
	}
	accountIDs := []string{testAccountID, "bb1e9a5d-37ad-43f8-83bd-13de8168f8af"}
	manager.newID = func() string {
		id := operationIDs[0]
		operationIDs = operationIDs[1:]
		return id
	}
	manager.catalog.newID = func() string {
		id := accountIDs[0]
		accountIDs = accountIDs[1:]
		return id
	}

	if err := manager.bootstrapInner(); err != nil {
		t.Fatal(err)
	}
	first := manager.activeAccountID()
	if first != testAccountID || state.active.Revision != 1 {
		t.Fatalf("first imported active account = %q, state=%#v", first, state.active)
	}
	if err := writeGlobalCredentialAtomic(globalCredential, []byte("credential-b")); err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcileGlobal(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := manager.activeAccountID()
	if second != "bb1e9a5d-37ad-43f8-83bd-13de8168f8af" || state.active.Revision != 2 {
		t.Fatalf("external login was not imported as active: %q state=%#v", second, state.active)
	}
	if snapshots := manager.catalog.snapshots(); len(snapshots) != 2 {
		t.Fatalf("accounts after external login = %#v", snapshots)
	}
	stored, err := readOpaqueCredential(filepath.Join(manager.catalog.root, second, codexCredentialHomeDirectory, codexCredentialFilename))
	if err != nil || string(stored) != "credential-b" {
		t.Fatalf("imported external credential = %q, err=%v", stored, err)
	}
}

func TestUnmanagedGlobalCredentialDoesNotBlockNormalAuthentication(t *testing.T) {
	root := t.TempDir()
	globalHome := filepath.Join(root, "global-codex")
	if err := ensurePrivateDirectory(globalHome); err != nil {
		t.Fatal(err)
	}
	email := "keyring@example.com"
	factory := &fakeCodexAccountFactory{
		capabilities: supportedCodexAccountCapabilities(),
		open: func(account ports.CodexAccountContext) (ports.CodexAccountClient, error) {
			if account.Managed {
				t.Fatal("keyring-backed global account was copied into a managed home")
			}
			return &fakeCodexAccountClient{read: ports.CodexAccountObservation{Authentication: domain.AgentAuthenticationAuthorized, Method: domain.CodexAuthMethodChatGPT, Email: &email}}, nil
		},
	}
	manager := newCodexAccountManager(context.Background(), filepath.Join(root, "accounts"), filepath.Join(root, "pending"), filepath.Join(root, "staging"), globalHome, factory, nil, nil)
	if err := manager.bootstrapInner(); err != nil {
		t.Fatal(err)
	}
	view := manager.cached()
	if view.ActiveAccountID != "" || view.UnmanagedGlobalAccount == nil || view.UnmanagedGlobalAccount.AccountEmail == nil || *view.UnmanagedGlobalAccount.AccountEmail != email {
		t.Fatalf("unmanaged global account = %#v", view)
	}
	if got := manager.detectCapabilities(context.Background()).GlobalSwitch.State; got != domain.CodexCapabilityUnsupported {
		t.Fatalf("global switch capability = %q, want unsupported", got)
	}
	manager.mu.Lock()
	manager.bootstrapped = true
	manager.mu.Unlock()
	service := &Service{codexAccounts: manager}
	auth, handled := service.structuredCodexAuthentication(context.Background(), string(domain.HarnessCodex), domain.AgentReadinessPurposeDisplay)
	if !handled || auth.State != domain.AgentAuthenticationAuthorized {
		t.Fatalf("normal Codex authentication = (%#v, %v), want authorized", auth, handled)
	}
}

func TestCredentialActivationDoesNotOverwriteExternalRace(t *testing.T) {
	root := t.TempDir()
	globalHome := filepath.Join(root, "global-codex")
	if err := ensurePrivateDirectory(globalHome); err != nil {
		t.Fatal(err)
	}
	globalPath := filepath.Join(globalHome, codexCredentialFilename)
	if err := writePrivateFileAtomic(globalPath, []byte("source-credential")); err != nil {
		t.Fatal(err)
	}
	sourceEmail, targetEmail := "source@example.com", "target@example.com"
	state := &fakeCodexAccountStateStore{active: domain.CodexActiveAccount{AccountID: testAccountID, Revision: 1}, found: true}
	manager := newCodexAccountManager(context.Background(), filepath.Join(root, "accounts"), filepath.Join(root, "pending"), filepath.Join(root, "staging"), globalHome, nil, state, nil)
	accountIDs := []string{testAccountID, "bb1e9a5d-37ad-43f8-83bd-13de8168f8af"}
	manager.catalog.newID = func() string {
		id := accountIDs[0]
		accountIDs = accountIDs[1:]
		return id
	}
	source := commitTestAccount(t, manager.catalog, manager.pendingRoot, "b60a377d-da68-4a61-86f2-f31f04c571f2", ports.CodexAccountObservation{Authentication: domain.AgentAuthenticationAuthorized, Method: domain.CodexAuthMethodChatGPT, Email: &sourceEmail})
	target := commitTestAccount(t, manager.catalog, manager.pendingRoot, "1c5de3ab-82d0-4a68-a06b-8495cdeab909", ports.CodexAccountObservation{Authentication: domain.AgentAuthenticationAuthorized, Method: domain.CodexAuthMethodChatGPT, Email: &targetEmail})
	if err := writePrivateFileAtomic(filepath.Join(source.Home, codexCredentialFilename), []byte("source-credential")); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateFileAtomic(filepath.Join(target.Home, codexCredentialFilename), []byte("target-credential")); err != nil {
		t.Fatal(err)
	}
	manager.active = state.active
	manager.factory = &fakeCodexAccountFactory{open: func(account ports.CodexAccountContext) (ports.CodexAccountClient, error) {
		if account.Home != globalHome || account.Managed {
			t.Fatalf("activation context = %#v", account)
		}
		if err := writeGlobalCredentialAtomic(globalPath, []byte("external-credential")); err != nil {
			t.Fatal(err)
		}
		return &fakeCodexAccountClient{read: ports.CodexAccountObservation{Authentication: domain.AgentAuthenticationUnauthorized}}, nil
	}}
	_, err := manager.activateFromCredential(context.Background(), target.Snapshot.ID, 1, filepath.Join(target.Home, codexCredentialFilename), []byte("source-credential"))
	if !errors.Is(err, ports.ErrCodexGlobalAccountChanged) {
		t.Fatalf("activation error = %v, want global-account-changed", err)
	}
	current, readErr := readOpaqueCredential(globalPath)
	if readErr != nil || string(current) != "external-credential" {
		t.Fatalf("external credential was overwritten: %q, err=%v", current, readErr)
	}
	if state.active.AccountID != source.Snapshot.ID || state.active.Revision != 1 {
		t.Fatalf("active pointer changed during race: %#v", state.active)
	}
}

func TestConsumeResetCreditVerifiesAvailabilityAndRefreshesCapacity(t *testing.T) {
	now := time.Now().UTC()
	available := &domain.CodexResetCreditsSummary{AvailableCount: 1}
	client := &fakeCodexAccountClient{
		read: ports.CodexAccountObservation{Authentication: domain.AgentAuthenticationAuthorized, Method: domain.CodexAuthMethodChatGPT},
		capacity: ports.CodexCapacityObservation{
			ObservedAt:   now,
			Overall:      &domain.CodexCapacityBucket{LimitID: "codex", Reached: domain.CodexCapacityReached, Primary: &domain.CodexCapacityWindow{UsedPercent: 100}},
			ResetCredits: available,
		},
	}
	client.resetFn = func(string) (domain.CodexResetCreditOutcome, error) {
		client.capacity = ports.CodexCapacityObservation{
			ObservedAt:   now.Add(time.Second),
			Overall:      &domain.CodexCapacityBucket{LimitID: "codex", Reached: domain.CodexCapacityNotReached, Primary: &domain.CodexCapacityWindow{UsedPercent: 0}},
			ResetCredits: &domain.CodexResetCreditsSummary{AvailableCount: 0},
		}
		return domain.CodexResetCreditReset, nil
	}
	factory := &fakeCodexAccountFactory{capabilities: supportedCodexAccountCapabilities(), open: func(ports.CodexAccountContext) (ports.CodexAccountClient, error) { return client, nil }}
	manager := newTestCodexAccountManager(t, factory, nil)
	manager.catalog.newID = func() string { return testAccountID }
	record := commitTestAccount(t, manager.catalog, manager.pendingRoot, "b60a377d-da68-4a61-86f2-f31f04c571f2", ports.CodexAccountObservation{Authentication: domain.AgentAuthenticationAuthorized, Method: domain.CodexAuthMethodChatGPT})
	if err := manager.consumeResetCredit(context.Background(), record.Snapshot.ID, "reset-request-1"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(client.resetKeys, []string{"reset-request-1"}) {
		t.Fatalf("reset keys = %#v", client.resetKeys)
	}
	snapshot := manager.capacity.snapshot(record.Snapshot.ID)
	if snapshot.State != domain.CodexCapacityAvailable || snapshot.RemainingPercent == nil || *snapshot.RemainingPercent != 100 || snapshot.ResetCredits == nil || snapshot.ResetCredits.AvailableCount != 0 {
		t.Fatalf("capacity after reset = %#v", snapshot)
	}
}

func TestAuthenticationRequestCancellationDoesNotCancelSharedRead(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	client := &fakeCodexAccountClient{read: ports.CodexAccountObservation{Authentication: domain.AgentAuthenticationAuthorized}, readStarted: started, readRelease: release}
	factory := &fakeCodexAccountFactory{open: func(ports.CodexAccountContext) (ports.CodexAccountClient, error) { return client, nil }}
	manager := newTestCodexAccountManager(t, factory, nil)
	manager.catalog.newID = func() string { return testAccountID }
	record := commitTestAccount(t, manager.catalog, manager.pendingRoot, "b60a377d-da68-4a61-86f2-f31f04c571f2", ports.CodexAccountObservation{Authentication: domain.AgentAuthenticationAuthorized, Method: domain.CodexAuthMethodChatGPT})
	waitCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := manager.ensureAuthentication(waitCtx, record, domain.AgentReadinessPurposeDisplay, false)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error = %v", err)
	}
	close(release)
	deadline := time.After(time.Second)
	for {
		latest, _ := manager.catalog.record(record.Snapshot.ID)
		if latest.Snapshot.Authentication.State == domain.AgentAuthenticationAuthorized {
			break
		}
		select {
		case <-deadline:
			t.Fatal("shared authentication read did not finish")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}
