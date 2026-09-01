import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const { getMock, postMock } = vi.hoisted(() => ({ getMock: vi.fn(), postMock: vi.fn() }));
vi.mock("../lib/api-client", () => ({
	apiClient: { GET: getMock, POST: postMock },
	apiErrorMessage: () => "request failed",
}));

import { codexAccountsQueryKey, useCodexAccountsQuery, useEnsureCodexAccounts } from "./useCodexAccountsQuery";

const response = {
	activeAccountId: null,
	accountRevision: 0,
	accounts: [],
	capabilities: {
		accountRead: { state: "supported", reasonCode: "supported", reason: "available" },
		nativeLogin: { state: "supported", reasonCode: "supported", reason: "available" },
		capacityRead: { state: "supported", reasonCode: "supported", reason: "available" },
		usageRead: { state: "unsupported", reasonCode: "unsupported", reason: "unavailable" },
		threadResume: { state: "supported", reasonCode: "supported", reason: "available" },
		accountManagement: { state: "supported", reasonCode: "supported", reason: "available" },
		globalSwitch: { state: "supported", reasonCode: "supported", reason: "available" },
	},
};

function wrapper(queryClient: QueryClient) {
	return ({ children }: { children: ReactNode }) => <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

beforeEach(() => {
	getMock.mockReset().mockResolvedValue({ data: response });
	postMock.mockReset().mockResolvedValue({ data: response });
});

describe("Codex account query", () => {
	it("reads the cached endpoint without starting native work", async () => {
		const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		const { result } = renderHook(() => useCodexAccountsQuery(), { wrapper: wrapper(queryClient) });
		await waitFor(() => expect(result.current.isSuccess).toBe(true));
		expect(getMock).toHaveBeenCalledWith("/api/v1/agents/codex/accounts");
		expect(postMock).not.toHaveBeenCalled();
	});

	it("ensures on surface open, focus, and visibility without polling", async () => {
		const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		queryClient.setQueryData(codexAccountsQueryKey, response);
		renderHook(() => useEnsureCodexAccounts(), { wrapper: wrapper(queryClient) });
		await waitFor(() => expect(postMock).toHaveBeenCalledTimes(1));
		expect(postMock).toHaveBeenLastCalledWith("/api/v1/agents/codex/accounts/ensure", {
			body: { accountIds: [], includeUsage: false },
		});
		const setIntervalSpy = vi.spyOn(window, "setInterval");
		await act(async () => {
			window.dispatchEvent(new Event("focus"));
			await Promise.resolve();
			await Promise.resolve();
		});
		expect(postMock).toHaveBeenCalledTimes(2);
		Object.defineProperty(document, "visibilityState", { configurable: true, value: "visible" });
		await act(async () => {
			document.dispatchEvent(new Event("visibilitychange"));
			await Promise.resolve();
			await Promise.resolve();
		});
		expect(postMock).toHaveBeenCalledTimes(3);
		expect(setIntervalSpy).not.toHaveBeenCalled();
		setIntervalSpy.mockRestore();
	});
});
