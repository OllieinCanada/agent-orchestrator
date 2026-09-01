import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it, vi } from "vitest";
import { useUiStore } from "../../stores/ui-store";
import { CodexAccountsSection } from "./CodexAccountsSection";

const { getMock, postMock, scrollIntoViewMock, terminalStateCallback } = vi.hoisted(() => ({
	getMock: vi.fn(),
	postMock: vi.fn(),
	scrollIntoViewMock: vi.fn(),
	terminalStateCallback: { value: undefined as ((state: "exited" | "error") => void) | undefined },
}));

vi.mock("../../lib/api-client", () => ({
	apiClient: { GET: getMock, POST: postMock },
	apiErrorMessage: (error: unknown) => error instanceof Error ? error.message : "request failed",
}));

vi.mock("../TerminalPane", () => ({
	TerminalPane: ({ onTerminalStateChange }: { onTerminalStateChange?: (state: "exited" | "error") => void }) => {
		terminalStateCallback.value = onTerminalStateChange;
		return <div data-testid="inline-terminal-body" />;
	},
}));

const capability = (state = "supported") => ({ state, reasonCode: state, reason: state === "supported" ? "Available." : "Unavailable." });
const authentication = { state: "authorized", freshness: "fresh", checkedAt: "2026-08-31T10:00:00Z", attemptedAt: "2026-08-31T10:00:00Z", reasonCode: "authorized", reason: "Codex is signed in." };
const capacity = { state: "available", freshness: "fresh", plan: "pro", usedPercent: 4, remainingPercent: 96, resetsAt: null, observedAt: "2026-08-31T10:00:00Z", checkedAt: "2026-08-31T10:00:00Z", attemptedAt: "2026-08-31T10:00:00Z", reasonCode: "capacity_available", reason: "Capacity is available.", overall: null, additionalBuckets: [] };
const activeAccount = { id: "11111111-1111-4111-8111-111111111111", label: "active@example.com", source: "managed", status: "valid", reasonCode: "account_valid", reason: "Available.", active: true, authentication, authMethod: "chatgpt", accountEmail: "active@example.com", capacity, createdAt: "2026-08-31T09:00:00Z" };
const inactiveAccount = { ...activeAccount, id: "22222222-2222-4222-8222-222222222222", label: "other@example.com", accountEmail: "other@example.com", active: false, createdAt: "2026-08-31T09:05:00Z" };
const accountResponse = {
	activeAccountId: activeAccount.id,
	accountRevision: 3,
	accounts: [activeAccount, inactiveAccount],
	capabilities: {
		accountRead: capability(), nativeLogin: capability(), capacityRead: capability(), usageRead: capability("unsupported"),
		resetCreditConsume: capability(), threadResume: capability(), accountManagement: capability(), globalSwitch: capability(),
	},
};
const pendingLogin = {
	operation: { operationId: "login-1", status: "pending", reasonCode: "login_pending", reason: "Waiting for Codex sign-in.", expiresAt: "2026-08-31T10:15:00Z" },
	shellTerminal: { handleId: "shellterm-login-1", title: "Add Codex account", createdAt: "2026-08-31T10:00:00Z" },
};

function renderSection() {
	const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return { queryClient, ...render(<QueryClientProvider client={queryClient}><CodexAccountsSection /></QueryClientProvider>) };
}

beforeEach(() => {
	Object.defineProperty(HTMLElement.prototype, "scrollIntoView", { configurable: true, value: scrollIntoViewMock });
	scrollIntoViewMock.mockReset();
	terminalStateCallback.value = undefined;
	useUiStore.setState({ settingsModal: { scope: "global", section: "agents" }, codexAccountLoginTerminal: null });
	getMock.mockReset().mockResolvedValue({ data: accountResponse });
	postMock.mockReset().mockImplementation((path: string) => {
		if (path === "/api/v1/agents/codex/accounts/ensure") return Promise.resolve({ data: accountResponse });
		if (path === "/api/v1/agents/codex/accounts/login-terminal") return Promise.resolve({ data: pendingLogin });
		return Promise.resolve({ data: {} });
	});
});

it("shows active-first account cards with correct remaining capacity", async () => {
	renderSection();
	expect(await screen.findByText("active@example.com")).toBeInTheDocument();
	expect(screen.getByText("In use")).toBeInTheDocument();
	expect(screen.getAllByText(/96% remaining/).length).toBeGreaterThan(0);
	expect(screen.queryByText(/The selected account is the device/)).not.toBeInTheDocument();
	expect(screen.queryByText(/credential/i)).not.toBeInTheDocument();
	expect(screen.queryByText(/billing/i)).not.toBeInTheDocument();
});

it("preserves the complete account list when expanding accounts performs targeted ensures", async () => {
	postMock.mockImplementation((path: string, request?: { body?: { accountIds?: string[] } }) => {
		if (path !== "/api/v1/agents/codex/accounts/ensure") return Promise.resolve({ data: {} });
		const ids = request?.body?.accountIds ?? [];
		if (ids.length === 0) return Promise.resolve({ data: accountResponse });
		return Promise.resolve({ data: { ...accountResponse, accounts: accountResponse.accounts.filter((account) => ids.includes(account.id)) } });
	});
	const { container } = renderSection();
	expect(await screen.findByText("active@example.com")).toBeInTheDocument();
	expect(screen.getByText("other@example.com")).toBeInTheDocument();

	fireEvent.click(container.querySelector(`[data-account-id="${activeAccount.id}"] button`) as HTMLButtonElement);
	await waitFor(() => expect(postMock).toHaveBeenCalledWith(
		"/api/v1/agents/codex/accounts/ensure",
		{ body: { accountIds: [activeAccount.id], includeUsage: true } },
	));
	expect(screen.getByText("active@example.com")).toBeInTheDocument();
	expect(screen.getByText("other@example.com")).toBeInTheDocument();

	fireEvent.click(container.querySelector(`[data-account-id="${inactiveAccount.id}"] button`) as HTMLButtonElement);
	await waitFor(() => expect(postMock).toHaveBeenCalledWith(
		"/api/v1/agents/codex/accounts/ensure",
		{ body: { accountIds: [inactiveAccount.id], includeUsage: true } },
	));
	expect(screen.getByText("active@example.com")).toBeInTheDocument();
	expect(screen.getByText("other@example.com")).toBeInTheDocument();
});

it("does not offer switching when the device account has no reconciled source", async () => {
	const unreconciledAccount = { ...activeAccount, active: false };
	const unreconciledResponse = {
		...accountResponse,
		activeAccountId: undefined,
		accounts: [unreconciledAccount],
		unmanagedGlobalAccount: {
			label: activeAccount.label,
			authMethod: activeAccount.authMethod,
			accountEmail: activeAccount.accountEmail,
			reasonCode: "global_credential_store_unsupported",
			reason: "This Codex account is active on the device, but its credential store cannot be switched safely.",
		},
	};
	getMock.mockResolvedValue({ data: unreconciledResponse });
	postMock.mockResolvedValue({ data: unreconciledResponse });

	renderSection();
	expect(await screen.findByText(unreconciledResponse.unmanagedGlobalAccount.reason)).toBeInTheDocument();
	expect(screen.queryByRole("button", { name: "Switch to this account" })).not.toBeInTheDocument();
});

it("presents plan, general and model usage limits with remaining-capacity meters", async () => {
	const detailedAccount = {
		...activeAccount,
		capacity: {
			...capacity,
			usedPercent: 19,
			remainingPercent: 81,
			resetsAt: "2026-09-07T02:32:14Z",
			overall: {
				limitId: "codex",
				reached: "not_reached",
				primary: { usedPercent: 19, windowDurationMinutes: 10080, resetsAt: "2026-09-07T02:32:14Z" },
			},
			additionalBuckets: [{
				limitId: "spark-internal",
				displayName: "GPT-5.3-Codex-Spark",
				reached: "not_reached",
				primary: { usedPercent: 0, windowDurationMinutes: 300, resetsAt: "2026-08-31T21:09:40Z" },
				secondary: { usedPercent: 0, windowDurationMinutes: 10080, resetsAt: "2026-09-07T16:32:40Z" },
			}],
		},
		usageSummary: {
			latestDayTokens: 34904480,
			latestDayStartDate: "2026-08-31",
			lifetimeTokens: 54571452296,
			peakDailyTokens: 2000000000,
			longestRunningTurnSeconds: 26340,
			currentStreakDays: 2,
			longestStreakDays: 99,
			observedAt: "2026-08-31T10:00:00Z",
		},
	};
	const detailedResponse = { ...accountResponse, accounts: [detailedAccount, inactiveAccount] };
	getMock.mockResolvedValue({ data: detailedResponse });
	postMock.mockImplementation((path: string) => path === "/api/v1/agents/codex/accounts/ensure"
		? Promise.resolve({ data: detailedResponse })
		: Promise.resolve({ data: {} }));
	const { container } = renderSection();
	await screen.findAllByText(/81% remaining/);
	fireEvent.click(container.querySelector(`[data-account-id="${activeAccount.id}"] button`) as HTMLButtonElement);

	expect(await screen.findByRole("region", { name: "Your plan" })).toBeInTheDocument();
	expect(screen.getByRole("region", { name: "Activity" })).toBeInTheDocument();
	expect(screen.getByText("Pro plan")).toBeInTheDocument();
	expect(screen.getByText("General usage limits")).toBeInTheDocument();
	expect(screen.getAllByText("Weekly usage limit")).toHaveLength(2);
	expect(screen.getByText("GPT-5.3-Codex-Spark usage limits")).toBeInTheDocument();
	expect(screen.getByText("5-hour usage limit")).toBeInTheDocument();
	const weeklyMeter = screen.getByRole("progressbar", { name: /Weekly usage limit, 81% left/ });
	expect(weeklyMeter).toHaveAttribute("aria-valuenow", "81");
	expect(screen.queryByText("34.9M tokens")).not.toBeInTheDocument();
	expect(screen.getByText("54.6B tokens")).toBeInTheDocument();
	expect(screen.getByText("2B tokens")).toBeInTheDocument();
	expect(screen.getByText("7h 19m")).toBeInTheDocument();
	expect(screen.getByText("2 days")).toBeInTheDocument();
	expect(screen.getByText("99 days")).toBeInTheDocument();
	const activityMetrics = screen.getByTestId("codex-account-activity-metrics");
	expect(activityMetrics.children).toHaveLength(5);
	expect(activityMetrics).toHaveStyle({ gridTemplateColumns: "repeat(5, minmax(0, 1fr))" });
	expect(activityMetrics.parentElement).not.toHaveClass("overflow-x-auto");
	expect(screen.queryByText("19% used")).not.toBeInTheDocument();
	expect(screen.queryByText("54571452296")).not.toBeInTheDocument();
});

it("shows provider-reported resets and confirms before consuming one", async () => {
	const accountWithReset = {
		...activeAccount,
		capacity: {
			...capacity,
			resetCredits: { availableCount: 1, nearestExpiresAt: "2026-09-21T00:15:00Z" },
		},
	};
	const responseWithReset = { ...accountResponse, accounts: [accountWithReset, inactiveAccount] };
	const responseAfterReset = {
		...responseWithReset,
		accounts: [{ ...accountWithReset, capacity: { ...accountWithReset.capacity, resetCredits: { availableCount: 0 } } }, inactiveAccount],
	};
	vi.stubGlobal("crypto", { randomUUID: () => "reset-request-1" });
	getMock.mockResolvedValue({ data: responseWithReset });
	postMock.mockImplementation((path: string) => {
		if (path === "/api/v1/agents/codex/accounts/ensure") return Promise.resolve({ data: responseWithReset });
		if (path === "/api/v1/agents/codex/accounts/{accountId}/reset-credit/consume") return Promise.resolve({ data: responseAfterReset });
		return Promise.resolve({ data: {} });
	});
	const { container } = renderSection();
	await screen.findByText("active@example.com");
	fireEvent.click(container.querySelector(`[data-account-id="${activeAccount.id}"] button`) as HTMLButtonElement);
	expect(await screen.findByText("1 reset available")).toBeInTheDocument();
	fireEvent.click(screen.getByRole("button", { name: "Use reset" }));
	expect(await screen.findByText("Use a usage-limit reset?")).toBeInTheDocument();
	const resetButtons = screen.getAllByRole("button", { name: "Use reset" });
	fireEvent.click(resetButtons[resetButtons.length - 1]);
	await waitFor(() => expect(postMock).toHaveBeenCalledWith(
		"/api/v1/agents/codex/accounts/{accountId}/reset-credit/consume",
		{ params: { path: { accountId: activeAccount.id } }, body: { idempotencyKey: "reset-request-1" } },
	));
	await waitFor(() => expect(screen.getByText("No resets available")).toBeInTheDocument());
	vi.unstubAllGlobals();
});

it("uses safe fallback headings and preserves stale values without exposing raw limit ids", async () => {
	const staleAccount = {
		...activeAccount,
		capacity: {
			...capacity,
			freshness: "stale",
			checkedAt: "2026-08-31T10:00:00Z",
			overall: null,
			additionalBuckets: [{
				limitId: "provider-secret-bucket-id",
				reached: "not_reached",
				primary: { usedPercent: 75, windowDurationMinutes: 60, resetsAt: null },
			}],
		},
	};
	const staleResponse = { ...accountResponse, accounts: [staleAccount] };
	getMock.mockResolvedValue({ data: staleResponse });
	postMock.mockResolvedValue({ data: staleResponse });
	const { container } = renderSection();
	await screen.findByText("active@example.com");
	fireEvent.click(container.querySelector(`[data-account-id="${activeAccount.id}"] button`) as HTMLButtonElement);

	expect(await screen.findByText("Additional usage limits")).toBeInTheDocument();
	expect(screen.queryByText("provider-secret-bucket-id")).not.toBeInTheDocument();
	expect(screen.getByRole("status")).toHaveTextContent(/Usage information may be out of date/);
	expect(screen.getByRole("progressbar")).toHaveAttribute("aria-valuenow", "25");
});

it("collapses the provider while rotating only its chevron", async () => {
	renderSection();
	await screen.findByText("active@example.com");
	const providerToggle = screen.getByRole("button", { name: /Codex/ });
	const icon = providerToggle.querySelector("img");
	const chevron = providerToggle.querySelector("svg");
	expect(icon).not.toBeNull();
	expect(chevron).not.toBeNull();
	fireEvent.click(providerToggle);
	expect(screen.queryByText("active@example.com")).not.toBeInTheDocument();
	expect(icon?.getAttribute("class")).not.toContain("rotate");
	expect(chevron?.getAttribute("class")).toContain("rotate");
});

it("starts account login immediately with no name prompt and auto-scrolls the inline terminal", async () => {
	renderSection();
	await screen.findByText("active@example.com");
	fireEvent.click(screen.getByRole("button", { name: "Add account" }));
	await waitFor(() => expect(postMock).toHaveBeenCalledWith("/api/v1/agents/codex/accounts/login-terminal"));
	expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
	expect(await screen.findByTestId("inline-terminal-body")).toBeInTheDocument();
	expect(scrollIntoViewMock).toHaveBeenCalledWith({ behavior: "smooth", block: "nearest" });
	expect(useUiStore.getState().settingsModal).toEqual({ scope: "global", section: "agents" });
	expect(screen.getByRole("button", { name: "Add account" })).toBeDisabled();
});

it("surfaces account-service unavailability instead of loading forever", async () => {
	const unavailable = new Error("Codex account management is unavailable");
	getMock.mockResolvedValue({ error: unavailable });
	postMock.mockResolvedValue({ error: unavailable });
	renderSection();

	expect((await screen.findAllByText("Codex account management is unavailable", {}, { timeout: 3_000 })).length).toBeGreaterThan(0);
	expect(screen.queryByText("Loading Codex accounts…")).not.toBeInTheDocument();
	expect(screen.getByRole("button", { name: "Add account" })).toBeDisabled();
});

it("verifies exactly once on terminal exit and collapses after structured success", async () => {
	const completedAccount = { ...inactiveAccount, id: "33333333-3333-4333-8333-333333333333", label: "new@example.com", accountEmail: "new@example.com" };
	// The verified operation is enough to update the card immediately. A
	// follow-up cached-list refresh is best-effort and must not keep the dead
	// terminal open when it fails.
	getMock.mockResolvedValueOnce({ data: accountResponse }).mockRejectedValue(new Error("refresh unavailable"));
	postMock.mockImplementation((path: string) => {
		if (path === "/api/v1/agents/codex/accounts/ensure") return Promise.resolve({ data: accountResponse });
		if (path === "/api/v1/agents/codex/accounts/login-terminal") return Promise.resolve({ data: pendingLogin });
		if (path.includes("/verify")) return Promise.resolve({ data: { ...pendingLogin.operation, status: "completed", reasonCode: "login_completed", reason: "Codex account added.", account: completedAccount } });
		return Promise.resolve({ data: {} });
	});
	renderSection();
	await screen.findByText("active@example.com");
	fireEvent.click(screen.getByRole("button", { name: "Add account" }));
	await screen.findByTestId("inline-terminal-body");
	act(() => terminalStateCallback.value?.("exited"));
	await waitFor(() => expect(postMock.mock.calls.filter(([path]) => String(path).includes("/verify"))).toHaveLength(1));
	await waitFor(() => expect(screen.queryByTestId("inline-terminal-body")).not.toBeInTheDocument());
	expect(await screen.findByText("new@example.com")).toBeInTheDocument();
});

it("retains terminal output when verification is unauthorized", async () => {
	postMock.mockImplementation((path: string) => {
		if (path === "/api/v1/agents/codex/accounts/ensure") return Promise.resolve({ data: accountResponse });
		if (path === "/api/v1/agents/codex/accounts/login-terminal") return Promise.resolve({ data: pendingLogin });
		if (path.includes("/verify")) return Promise.resolve({ data: { ...pendingLogin.operation, status: "unauthorized", reasonCode: "login_unauthorized", reason: "Codex is still signed out." } });
		return Promise.resolve({ data: {} });
	});
	renderSection();
	await screen.findByText("active@example.com");
	fireEvent.click(screen.getByRole("button", { name: "Add account" }));
	await screen.findByTestId("inline-terminal-body");
	act(() => terminalStateCallback.value?.("exited"));
	expect(await screen.findByRole("button", { name: "Retry" })).toBeEnabled();
	expect(screen.getByTestId("inline-terminal-body")).toBeInTheDocument();
});

it("starts a global switch with the displayed account revision", async () => {
	const switchOperation = { id: "switch-1", sourceAccountId: activeAccount.id, targetAccountId: inactiveAccount.id, phase: "requested", reasonCode: "switch_requested", reason: "Preparing account switch.", canCancel: true, canRecover: false, sessions: [], createdAt: "2026-08-31T10:00:00Z", updatedAt: "2026-08-31T10:00:00Z" };
	vi.stubGlobal("crypto", { randomUUID: () => "idempotency-1" });
	postMock.mockImplementation((path: string) => {
		if (path === "/api/v1/agents/codex/accounts/ensure") return Promise.resolve({ data: accountResponse });
		if (path === "/api/v1/agents/codex/account-switches") return Promise.resolve({ data: switchOperation });
		return Promise.resolve({ data: pendingLogin });
	});
	renderSection();
	await screen.findByText("other@example.com");
	expect(screen.queryByRole("button", { name: "Switch to this account" })).not.toBeInTheDocument();
	await userEvent.click(screen.getByRole("button", { name: "Switch account" }));
	await userEvent.click(await screen.findByRole("menuitem", { name: /other@example.com/ }));
	const dialog = await screen.findByRole("dialog");
	fireEvent.click(within(dialog).getByRole("button", { name: "Switch account" }));
	await waitFor(() => expect(postMock).toHaveBeenCalledWith("/api/v1/agents/codex/account-switches", {
		body: { targetAccountId: inactiveAccount.id, expectedAccountRevision: 3, idempotencyKey: "idempotency-1" },
	}));
	vi.unstubAllGlobals();
});
