import { useEffect } from "react";
import { useQuery, useQueryClient, type QueryClient } from "@tanstack/react-query";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage } from "../lib/api-client";
import { codexAccountsQueryKey } from "./codex-account-cache";

export { CODEX_ACCOUNT_DAEMON_RESET_EVENT, codexAccountsQueryKey } from "./codex-account-cache";

export type CodexAccountsResponse = components["schemas"]["CodexAccountsResponse"];
export type CodexAccount = components["schemas"]["CodexAccountSnapshot"];
export type CodexAccountLoginOperation = components["schemas"]["CodexAccountLoginOperation"];
export type CodexAccountLoginTerminalStart = components["schemas"]["OpenCodexAccountLoginTerminalResponse"];
export type CodexAccountSwitch = components["schemas"]["CodexAccountSwitch"];

export async function fetchCodexAccounts(): Promise<CodexAccountsResponse> {
	const { data, error } = await apiClient.GET("/api/v1/agents/codex/accounts");
	if (error) throw new Error(apiErrorMessage(error));
	return data as CodexAccountsResponse;
}

export async function ensureCodexAccounts(accountIds: string[] = [], includeUsage = false): Promise<CodexAccountsResponse> {
	const { data, error } = await apiClient.POST("/api/v1/agents/codex/accounts/ensure", { body: { accountIds, includeUsage } });
	if (error) throw new Error(apiErrorMessage(error));
	return data as CodexAccountsResponse;
}

export async function consumeCodexAccountResetCredit(accountId: string, idempotencyKey: string): Promise<CodexAccountsResponse> {
	const { data, error } = await apiClient.POST("/api/v1/agents/codex/accounts/{accountId}/reset-credit/consume", {
		params: { path: { accountId } },
		body: { idempotencyKey },
	});
	if (error) throw new Error(apiErrorMessage(error));
	return data as CodexAccountsResponse;
}

export async function openCodexAccountLoginTerminal(): Promise<CodexAccountLoginTerminalStart> {
	const { data, error } = await apiClient.POST("/api/v1/agents/codex/accounts/login-terminal");
	if (error) throw new Error(apiErrorMessage(error));
	return data as CodexAccountLoginTerminalStart;
}

export async function verifyCodexAccountLogin(operationId: string): Promise<CodexAccountLoginOperation> {
	const { data, error } = await apiClient.POST("/api/v1/agents/codex/accounts/login-operations/{operationId}/verify", { params: { path: { operationId } } });
	if (error) throw new Error(apiErrorMessage(error));
	return data as CodexAccountLoginOperation;
}

export async function cancelCodexAccountLogin(operationId: string): Promise<CodexAccountLoginOperation> {
	const { data, error } = await apiClient.POST("/api/v1/agents/codex/accounts/login-operations/{operationId}/cancel", { params: { path: { operationId } } });
	if (error) throw new Error(apiErrorMessage(error));
	return data as CodexAccountLoginOperation;
}

export async function startCodexAccountSwitch(targetAccountId: string, expectedAccountRevision: number, idempotencyKey: string): Promise<CodexAccountSwitch> {
	const { data, error } = await apiClient.POST("/api/v1/agents/codex/account-switches", { body: { targetAccountId, expectedAccountRevision, idempotencyKey } });
	if (error) throw new Error(apiErrorMessage(error));
	return data as CodexAccountSwitch;
}

export async function recoverCodexAccountSwitch(switchId: string): Promise<CodexAccountSwitch> {
	const { data, error } = await apiClient.POST("/api/v1/agents/codex/account-switches/{switchId}/recover", { params: { path: { switchId } } });
	if (error) throw new Error(apiErrorMessage(error));
	return data as CodexAccountSwitch;
}

export function cacheCodexAccounts(queryClient: QueryClient, next: CodexAccountsResponse): void { queryClient.setQueryData(codexAccountsQueryKey, next); }

export function cacheCodexAccount(queryClient: QueryClient, account: CodexAccount): void {
	queryClient.setQueryData<CodexAccountsResponse>(codexAccountsQueryKey, (current) => {
		if (!current) return current;
		const accounts = current.accounts
			.filter((item) => item.id !== account.id)
			.map((item) => account.active ? { ...item, active: false } : item);
		accounts.push(account);
		accounts.sort((left, right) => {
			if (left.active !== right.active) return left.active ? -1 : 1;
			const created = left.createdAt.localeCompare(right.createdAt);
			return created || left.id.localeCompare(right.id);
		});
		return {
			...current,
			activeAccountId: account.active ? account.id : current.activeAccountId,
			accounts,
		};
	});
}

export const codexAccountsQueryOptions = { queryKey: codexAccountsQueryKey, queryFn: fetchCodexAccounts, retry: 1, staleTime: Number.POSITIVE_INFINITY };
export function useCodexAccountsQuery(enabled = true) { return useQuery({ ...codexAccountsQueryOptions, enabled }); }

export function useEnsureCodexAccounts(enabled = true): void {
	const queryClient = useQueryClient();
	useEffect(() => {
		if (!enabled) return;
		let active = true;
		const ensure = () => {
			const cached = queryClient.getQueryData(codexAccountsQueryKey);
			const ready = cached ? Promise.resolve() : queryClient.fetchQuery(codexAccountsQueryOptions).then(() => undefined).catch(() => undefined);
			void ready.then(() => ensureCodexAccounts()).then((next) => { if (active) cacheCodexAccounts(queryClient, next); }).catch(() => undefined);
		};
		ensure();
		const onFocus = () => ensure();
		const onVisibility = () => { if (document.visibilityState === "visible") ensure(); };
		window.addEventListener("focus", onFocus); document.addEventListener("visibilitychange", onVisibility);
		return () => { active = false; window.removeEventListener("focus", onFocus); document.removeEventListener("visibilitychange", onVisibility); };
	}, [enabled, queryClient]);
}
