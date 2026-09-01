import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { useUiStore } from "../stores/ui-store";
import type { ProjectSettingsSaveState } from "./ProjectSettingsForm";
import { SettingsDialog } from "./SettingsDialog";

const { postMock } = vi.hoisted(() => ({ postMock: vi.fn() }));

vi.mock("../lib/api-client", () => ({
	apiClient: { POST: postMock },
	apiErrorCode: (error: { code?: string }) => error?.code,
	apiErrorMessage: () => "request failed",
	hasTrustedApiBaseUrl: () => true,
}));

vi.mock("./ProjectSettingsForm", () => ({
	ProjectSettingsForm: ({
		onSaveState,
	}: {
		onSaveState?: (state: ProjectSettingsSaveState) => void;
	}) => (
		<button
			type="button"
			onClick={() =>
				onSaveState?.({
					isPending: true,
					showSaving: false,
					validationError: null,
					mutationError: null,
					saved: false,
					replacementError: null,
				})
			}
		>
			Start pending save
		</button>
	),
}));

vi.mock("./GlobalSettingsForm", () => ({
	GlobalSettingsForm: ({ section }: { section: string }) => <div data-testid="global-settings-section">{section}</div>,
}));

// The dialog reads the cloud gate to decide whether the Cloud nav page exists;
// mocked so these tests need no QueryClientProvider (same pattern as Sidebar).
vi.mock("../hooks/useCloudGate", () => ({
	useCloudGate: () => ({ cloudEnabled: false, localEnabled: true }),
}));

describe("SettingsDialog", () => {
	beforeEach(() => {
		postMock.mockReset().mockResolvedValue({ data: { operationId: "login-1", status: "cancelled" } });
		useUiStore.setState({ settingsModal: null, codexAccountLoginTerminal: null });
	});

	function renderSettingsDialog() {
		const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		return render(<QueryClientProvider client={queryClient}><SettingsDialog /></QueryClientProvider>);
	}

	it("does not dismiss project settings while a save is pending", async () => {
		useUiStore.getState().openProjectSettings("proj-1");
		renderSettingsDialog();

		await userEvent.click(await screen.findByRole("button", { name: "Start pending save" }));
		const closeButton = screen.getByRole("button", { name: "Close settings" });
		expect(closeButton).toBeDisabled();

		await userEvent.keyboard("{Escape}");
		expect(useUiStore.getState().settingsModal).toEqual({ scope: "project", projectId: "proj-1" });
	});

	it("opens the requested global settings page", async () => {
		useUiStore.getState().openGlobalSettings("mobile");
		renderSettingsDialog();

		expect(await screen.findByTestId("global-settings-section")).toHaveTextContent("mobile");
		expect(screen.getByRole("button", { name: "Mobile" })).toHaveAttribute("aria-current", "page");
	});

	it("keeps Agents locked and destroys an active login terminal before closing", async () => {
		useUiStore.getState().openGlobalSettings("agents");
		useUiStore.getState().startCodexAccountLoginTerminal("login-1", {
			handleId: "shellterm-login-1",
			title: "Codex login",
			createdAt: "2026-08-29T12:00:00Z",
		});
		renderSettingsDialog();

		expect(await screen.findByTestId("global-settings-section")).toHaveTextContent("agents");
		expect(screen.getByRole("button", { name: "General" })).toBeDisabled();
		await userEvent.click(screen.getByRole("button", { name: "Close settings" }));

		await vi.waitFor(() => expect(postMock).toHaveBeenCalledWith("/api/v1/agents/codex/accounts/login-operations/{operationId}/cancel", {
			params: { path: { operationId: "login-1" } },
		}));
		await vi.waitFor(() => expect(useUiStore.getState().settingsModal).toBeNull());
		expect(useUiStore.getState().codexAccountLoginTerminal).toBeNull();
	});

	it("keeps settings open when active login terminal destruction fails", async () => {
		postMock.mockResolvedValue({ error: { code: "CODEX_ACCOUNT_MANAGEMENT_UNAVAILABLE" } });
		useUiStore.getState().openGlobalSettings("agents");
		useUiStore.getState().startCodexAccountLoginTerminal("login-1", {
			handleId: "shellterm-login-1",
			title: "Codex login",
			createdAt: "2026-08-29T12:00:00Z",
		});
		renderSettingsDialog();

		await userEvent.click(await screen.findByRole("button", { name: "Close settings" }));

		await vi.waitFor(() => expect(useUiStore.getState().codexAccountLoginTerminal?.reason).toContain("Settings will stay open"));
		expect(useUiStore.getState().settingsModal).toEqual({ scope: "global", section: "agents" });
	});

	it("closes settings when the login operation is already terminal", async () => {
		postMock.mockResolvedValue({ data: { operationId: "login-1", status: "cancelled" } });
		useUiStore.getState().openGlobalSettings("agents");
		useUiStore.getState().startCodexAccountLoginTerminal("login-1", {
			handleId: "shellterm-missing",
			title: "Codex login",
			createdAt: "2026-08-29T12:00:00Z",
		});
		renderSettingsDialog();

		await userEvent.click(await screen.findByRole("button", { name: "Close settings" }));

		await vi.waitFor(() => expect(useUiStore.getState().settingsModal).toBeNull());
		expect(useUiStore.getState().codexAccountLoginTerminal).toBeNull();
	});
});
