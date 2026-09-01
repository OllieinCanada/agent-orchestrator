import { ChevronDown, CircleAlert, CircleCheck, LoaderCircle, Plus, RotateCcw, UserRound, X } from "lucide-react";
import type { TFunction } from "i18next";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import {
	cacheCodexAccount,
	cacheCodexAccounts,
	cancelCodexAccountLogin,
	consumeCodexAccountResetCredit,
	ensureCodexAccounts,
	fetchCodexAccounts,
	mergeCodexAccounts,
	openCodexAccountLoginTerminal,
	recoverCodexAccountSwitch,
	startCodexAccountSwitch,
	useCodexAccountsQuery,
	useEnsureCodexAccounts,
	verifyCodexAccountLogin,
	type CodexAccount,
} from "../../hooks/useCodexAccountsQuery";
import { shellTerminalsQueryKey } from "../../hooks/useShellTerminals";
import type { TerminalSessionState } from "../../hooks/useTerminalSession";
import { useShellMaybe } from "../../lib/shell-context";
import {
	type CodexAccountLoginTerminalWorkflow,
	useResolvedTheme,
	useUiStore,
} from "../../stores/ui-store";
import { ConfirmDialog } from "../ConfirmDialog";
import { TerminalPane } from "../TerminalPane";
import { Button } from "../ui/button";
import { AgentProviderGroup } from "./AgentProviderGroup";
import { SettingsSection } from "./SettingsSection";

export function CodexAccountsSection({ titleHidden }: { titleHidden?: boolean }) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const accountsQuery = useCodexAccountsQuery();
	useEnsureCodexAccounts(true);
	const [providerExpanded, setProviderExpanded] = useState(true);
	const [expandedAccount, setExpandedAccount] = useState<string | null>(null);
	const [busy, setBusy] = useState<string | null>(null);
	const [confirmAccount, setConfirmAccount] = useState<CodexAccount | null>(null);
	const [confirmResetAccount, setConfirmResetAccount] = useState<CodexAccount | null>(null);
	const switchRequestRef = useRef<{ accountId: string; revision: number; idempotencyKey: string } | null>(null);
	const resetRequestRef = useRef<{ accountId: string; idempotencyKey: string } | null>(null);
	const [error, setError] = useState<string | null>(null);
	const [announcement, setAnnouncement] = useState("");
	const loginWorkflow = useUiStore((state) => state.codexAccountLoginTerminal);
	const startLoginTerminal = useUiStore((state) => state.startCodexAccountLoginTerminal);
	const updateLoginTerminal = useUiStore((state) => state.updateCodexAccountLoginTerminal);
	const clearLoginTerminal = useUiStore((state) => state.clearCodexAccountLoginTerminal);
	const data = accountsQuery.data;
	const accountsError = accountsQuery.error instanceof Error ? accountsQuery.error.message : null;
	const activeAccount = data?.accounts.find((account) => account.active);
	const currentSwitch = data?.currentSwitch;
	const resetCreditSupported = data?.capabilities.resetCreditConsume.state === "supported";
	const switchSourceAvailable = Boolean(data?.activeAccountId && activeAccount && !data.unmanagedGlobalAccount);
	const switchActive = Boolean(currentSwitch && !["completed", "cancelled", "failed"].includes(currentSwitch.phase));

	const beginLogin = useCallback(async () => {
		if (useUiStore.getState().codexAccountLoginTerminal || switchActive) return;
		setProviderExpanded(true);
		setBusy("login");
		setError(null);
		setAnnouncement("");
		try {
			const started = await openCodexAccountLoginTerminal();
			startLoginTerminal(started.operation.operationId, {
				handleId: started.shellTerminal.handleId,
				title: started.shellTerminal.title,
				createdAt: started.shellTerminal.createdAt,
			});
			void queryClient.invalidateQueries({ queryKey: shellTerminalsQueryKey });
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : t("settings.codexAccounts.loginFailed"));
		} finally {
			setBusy(null);
		}
	}, [queryClient, startLoginTerminal, switchActive, t]);

	const verifyLogin = useCallback(async (workflow: CodexAccountLoginTerminalWorkflow) => {
		const { handleId } = workflow.terminal;
		const current = useUiStore.getState().codexAccountLoginTerminal;
		if (!current || current.terminal.handleId !== handleId || current.phase === "verifying") return;
		updateLoginTerminal(handleId, { phase: "verifying", reason: undefined });
		try {
			const operation = await verifyCodexAccountLogin(workflow.operationId);
			if (useUiStore.getState().codexAccountLoginTerminal?.terminal.handleId !== handleId) return;
			// Verification can commit the account before first-account activation
			// finishes. Preserve that durable result in the list even if activation
			// returns a recoverable failure; retrying login would only create a
			// duplicate account for credentials AO already saved.
			if (operation.account) cacheCodexAccount(queryClient, operation.account);
			if (operation.status === "completed" && operation.account) {
				clearLoginTerminal(handleId);
				void queryClient.invalidateQueries({ queryKey: shellTerminalsQueryKey });
				setAnnouncement(t("settings.codexAccounts.loginSuccess", { label: operation.account.label }));
				window.requestAnimationFrame(() => document.getElementById(`codex-account-${operation.account?.id}`)?.focus());
				void fetchCodexAccounts()
					.then((next) => cacheCodexAccounts(queryClient, next))
					.catch(() => undefined);
				return;
			}
			if (operation.status === "failed" && operation.account) {
				clearLoginTerminal(handleId);
				void queryClient.invalidateQueries({ queryKey: shellTerminalsQueryKey });
				setError(operation.reason);
				window.requestAnimationFrame(() => document.getElementById(`codex-account-${operation.account?.id}`)?.focus());
				void fetchCodexAccounts()
					.then((next) => cacheCodexAccounts(queryClient, next))
					.catch(() => undefined);
				return;
			}
			const phase = operation.status === "unauthorized"
				? "unauthorized"
				: operation.status === "expired"
					? "expired"
					: operation.status === "failed"
						? "failed"
						: "unverified";
			updateLoginTerminal(handleId, { phase, reason: operation.reason });
		} catch (cause) {
			updateLoginTerminal(handleId, {
				phase: "unverified",
				reason: cause instanceof Error ? cause.message : t("settings.codexAccounts.loginVerificationFailed"),
			});
		}
	}, [clearLoginTerminal, queryClient, t, updateLoginTerminal]);

	const closeLogin = useCallback(async (workflow: CodexAccountLoginTerminalWorkflow) => {
		const { handleId } = workflow.terminal;
		updateLoginTerminal(handleId, { phase: "closing", reason: undefined });
		try {
			await cancelCodexAccountLogin(workflow.operationId);
			clearLoginTerminal(handleId);
			void queryClient.invalidateQueries({ queryKey: shellTerminalsQueryKey });
		} catch (cause) {
			updateLoginTerminal(handleId, {
				phase: workflow.phase,
				reason: cause instanceof Error ? cause.message : t("settings.codexAccounts.loginCloseFailed"),
			});
		}
	}, [clearLoginTerminal, queryClient, t, updateLoginTerminal]);

	const retryLogin = useCallback(async (workflow: CodexAccountLoginTerminalWorkflow) => {
		await closeLogin(workflow);
		if (!useUiStore.getState().codexAccountLoginTerminal) await beginLogin();
	}, [beginLogin, closeLogin]);

	const toggleAccount = useCallback((account: CodexAccount) => {
		const opening = expandedAccount !== account.id;
		setExpandedAccount(opening ? account.id : null);
		if (!opening) return;
		void ensureCodexAccounts([account.id], true)
			.then((next) => mergeCodexAccounts(queryClient, next))
			.catch(() => undefined);
	}, [expandedAccount, queryClient]);

	const confirmSwitch = useCallback(async () => {
		if (!confirmAccount || !data?.activeAccountId || data.unmanagedGlobalAccount) return;
		let request = switchRequestRef.current;
		if (!request || request.accountId !== confirmAccount.id || request.revision !== data.accountRevision) {
			request = {
				accountId: confirmAccount.id,
				revision: data.accountRevision,
				idempotencyKey: crypto.randomUUID(),
			};
			switchRequestRef.current = request;
		}
		setBusy(confirmAccount.id);
		setError(null);
		try {
			await startCodexAccountSwitch(request.accountId, request.revision, request.idempotencyKey);
			switchRequestRef.current = null;
			setConfirmAccount(null);
			cacheCodexAccounts(queryClient, await fetchCodexAccounts());
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : t("settings.codexAccounts.switchFailed"));
		} finally {
			setBusy(null);
		}
	}, [confirmAccount, data, queryClient, t]);

	const recoverSwitch = useCallback(async () => {
		if (!currentSwitch) return;
		setBusy("recover");
		try {
			await recoverCodexAccountSwitch(currentSwitch.id);
			cacheCodexAccounts(queryClient, await fetchCodexAccounts());
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : t("settings.codexAccounts.switchRecoveryFailed"));
		} finally {
			setBusy(null);
		}
	}, [currentSwitch, queryClient, t]);

	const confirmReset = useCallback(async () => {
		if (!confirmResetAccount || switchActive || loginWorkflow) return;
		let request = resetRequestRef.current;
		if (!request || request.accountId !== confirmResetAccount.id) {
			request = { accountId: confirmResetAccount.id, idempotencyKey: crypto.randomUUID() };
			resetRequestRef.current = request;
		}
		setBusy(`reset:${confirmResetAccount.id}`);
		setError(null);
		try {
			const next = await consumeCodexAccountResetCredit(request.accountId, request.idempotencyKey);
			cacheCodexAccounts(queryClient, next);
			resetRequestRef.current = null;
			setConfirmResetAccount(null);
			setAnnouncement(t("settings.codexAccounts.resetSuccess", { label: confirmResetAccount.label }));
		} catch (cause) {
			setError(cause instanceof Error ? cause.message : t("settings.codexAccounts.resetFailed"));
		} finally {
			setBusy(null);
		}
	}, [confirmResetAccount, loginWorkflow, queryClient, switchActive, t]);

	const summary = useMemo(() => {
		if (accountsError) return accountsError;
		if (!data) return t("settings.codexAccounts.loading");
		if (switchActive && currentSwitch) return currentSwitch.reason;
		if (activeAccount) {
			const remaining = activeAccount.capacity.remainingPercent;
			return [activeAccount.label, formatPlanName(activeAccount.capacity.plan), remaining == null ? null : `${formatPercentage(remaining)} ${t("settings.codexAccounts.remaining")}`].filter(Boolean).join(" · ");
		}
		if (data.unmanagedGlobalAccount) return data.unmanagedGlobalAccount.label;
		return t("settings.codexAccounts.count", { count: data.accounts.length });
	}, [accountsError, activeAccount, currentSwitch, data, switchActive, t]);

	return (
		<SettingsSection title={t("settings.codexAccounts.title")} sectionId="codex-accounts" titleHidden={titleHidden}>
			<AgentProviderGroup
				provider="codex"
				name="Codex"
				summary={summary}
				expanded={providerExpanded || Boolean(loginWorkflow)}
				onExpandedChange={setProviderExpanded}
				collapseLocked={Boolean(loginWorkflow)}
				action={(
					<div className="flex items-center gap-2">
						{switchActive ? <LoaderCircle className="size-5 animate-spin text-muted-foreground" aria-label={currentSwitch?.reason} /> : null}
						<Button type="button" size="sm" title={accountsError ?? undefined} onClick={() => void beginLogin()} disabled={Boolean(loginWorkflow) || switchActive || Boolean(busy) || data?.capabilities.nativeLogin.state !== "supported"}>
							<Plus aria-hidden="true" /> {t("settings.codexAccounts.add")}
						</Button>
					</div>
				)}
			>
				{error ? <p role="alert" className="border-b border-border px-4 py-3 text-xs text-error">{error}</p> : null}
				{data?.unmanagedGlobalAccount ? (
					<div className="border-b border-border px-4 py-3 text-xs">
						<p className="font-medium text-foreground">{data.unmanagedGlobalAccount.label}</p>
						<p className="mt-1 text-muted-foreground">{data.unmanagedGlobalAccount.reason}</p>
					</div>
				) : null}
				{announcement ? <p className="sr-only" role="status" aria-live="polite">{announcement}</p> : null}
				{loginWorkflow ? (
					<div className="border-b border-border px-4 py-3" data-testid="codex-account-pending-row">
						<CodexAccountLoginTerminalPanel
							workflow={loginWorkflow}
							onCheckAgain={() => void verifyLogin(loginWorkflow)}
							onClose={() => void closeLogin(loginWorkflow)}
							onRetry={() => void retryLogin(loginWorkflow)}
							onTerminalState={(state) => {
								if (state !== "exited" && state !== "error") return;
								const current = useUiStore.getState().codexAccountLoginTerminal;
								if (current?.terminal.handleId === loginWorkflow.terminal.handleId && current.phase === "running") void verifyLogin(current);
							}}
						/>
					</div>
				) : null}
				{accountsQuery.isLoading ? <p className="px-4 py-3 text-xs text-muted-foreground">{t("settings.codexAccounts.loading")}</p> : null}
				{accountsError ? <p className="px-4 py-3 text-xs text-error" role="alert">{accountsError}</p> : null}
				<div className="divide-y divide-border">
					{data?.accounts.map((account) => (
						<CodexAccountRow key={account.id} account={account} expanded={expandedAccount === account.id} switchSourceAvailable={switchSourceAvailable} resetCreditSupported={resetCreditSupported} mutationDisabled={Boolean(loginWorkflow) || switchActive || Boolean(busy)} switchUnavailableReason={data.capabilities.globalSwitch.state === "supported" ? undefined : data.capabilities.globalSwitch.reason} busy={busy === account.id} resetBusy={busy === `reset:${account.id}`} onToggle={() => toggleAccount(account)} onSwitch={() => { switchRequestRef.current = null; setConfirmAccount(account); }} onUseReset={() => setConfirmResetAccount(account)} />
					))}
				</div>
				{currentSwitch?.canRecover ? (
					<div className="border-t border-border px-4 py-3">
						<p className="text-xs text-error">{currentSwitch.reason}</p>
						<Button className="mt-2" type="button" size="sm" variant="outline" disabled={busy === "recover"} onClick={() => void recoverSwitch()}>{t("settings.codexAccounts.retryRestart")}</Button>
					</div>
				) : null}
			</AgentProviderGroup>
			<ConfirmDialog open={Boolean(confirmAccount && switchSourceAvailable)} title={t("settings.codexAccounts.switchTitle")} description={t("settings.codexAccounts.switchDescription", { label: confirmAccount?.label ?? "" })} confirmLabel={t("settings.codexAccounts.switchConfirm")} busy={Boolean(busy)} onConfirm={() => void confirmSwitch()} onOpenChange={(open) => { if (!open && !busy) { switchRequestRef.current = null; setConfirmAccount(null); } }} />
			<ConfirmDialog open={Boolean(confirmResetAccount)} title={t("settings.codexAccounts.resetTitle")} description={t("settings.codexAccounts.resetDescription", { label: confirmResetAccount?.label ?? "" })} confirmLabel={t("settings.codexAccounts.useReset")} busy={Boolean(busy)} onConfirm={() => void confirmReset()} onOpenChange={(open) => { if (!open && !busy) setConfirmResetAccount(null); }} />
		</SettingsSection>
	);
}

function CodexAccountRow({ account, expanded, switchSourceAvailable, resetCreditSupported, mutationDisabled, switchUnavailableReason, busy, resetBusy, onToggle, onSwitch, onUseReset }: {
	account: CodexAccount;
	expanded: boolean;
	switchSourceAvailable: boolean;
	resetCreditSupported: boolean;
	mutationDisabled: boolean;
	switchUnavailableReason?: string;
	busy: boolean;
	resetBusy: boolean;
	onToggle: () => void;
	onSwitch: () => void;
	onUseReset: () => void;
}) {
	const { t } = useTranslation();
	const authorized = account.authentication.state === "authorized" || account.authentication.state === "not_applicable";
	const remaining = account.capacity.remainingPercent;
	const authenticationLabel = authorized
		? account.accountEmail && account.accountEmail !== account.label
			? account.accountEmail
			: t("settings.codexAccounts.signedIn")
		: t("settings.codexAccounts.unknown");
	const summary = [formatAuthMethod(account.authMethod), formatPlanName(account.capacity.plan), remaining == null ? null : `${formatPercentage(remaining)} ${t("settings.codexAccounts.remaining")}`].filter(Boolean).join(" · ");
	return (
		<div id={`codex-account-${account.id}`} data-account-id={account.id} tabIndex={-1} className="px-4 py-3 outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring">
			<div className="flex items-start justify-between gap-3">
				<button type="button" className="flex min-w-0 flex-1 items-start gap-3 rounded-sm text-left focus:outline-none focus-visible:ring-2 focus-visible:ring-ring" aria-expanded={expanded} onClick={onToggle}>
					<UserRound data-testid="codex-account-avatar" className="mt-0.5 size-6 shrink-0 text-muted-foreground" aria-hidden="true" />
					<div className="min-w-0">
						<div className="flex items-center gap-2"><p className="truncate text-sm font-medium">{account.label}</p>{account.active ? <span className="rounded-full border border-success/30 bg-success/10 px-2 py-0.5 text-[10px] font-medium text-success">{t("settings.codexAccounts.inUse")}</span> : null}</div>
						<p className="mt-1 flex items-center gap-1 text-xs text-muted-foreground">{authorized ? <CircleCheck className="size-3.5 text-success" aria-hidden="true" /> : <CircleAlert className="size-3.5" aria-hidden="true" />}{authenticationLabel}{account.authentication.freshness === "checking" ? <LoaderCircle className="size-3.5 animate-spin" aria-label={t("settings.codexAccounts.checking")} /> : null}</p>
						{summary ? <p className="mt-1 truncate text-xs text-muted-foreground">{summary}</p> : null}
					</div>
					<ChevronDown className={`ml-auto mt-1 size-4 shrink-0 text-muted-foreground transition-transform ${expanded ? "" : "-rotate-90"}`} aria-hidden="true" />
				</button>
				{switchSourceAvailable && !account.active && account.status === "valid" && authorized ? <Button type="button" size="sm" variant="outline" disabled={mutationDisabled || busy || Boolean(switchUnavailableReason)} title={switchUnavailableReason} onClick={onSwitch}>{t("settings.codexAccounts.switchAction")}</Button> : null}
			</div>
			{expanded ? <CodexAccountDetails account={account} resetCreditSupported={resetCreditSupported} mutationDisabled={mutationDisabled} resetBusy={resetBusy} onUseReset={onUseReset} /> : null}
		</div>
	);
}

function CodexAccountDetails({ account, resetCreditSupported, mutationDisabled, resetBusy, onUseReset }: { account: CodexAccount; resetCreditSupported: boolean; mutationDisabled: boolean; resetBusy: boolean; onUseReset: () => void }) {
	const { t, i18n } = useTranslation();
	const plan = formatPlanLabel(account.capacity.plan, t);
	const hasOverall = Boolean(account.capacity.overall?.primary || account.capacity.overall?.secondary);
	const additionalBuckets = account.capacity.additionalBuckets.filter((bucket) => bucket.primary || bucket.secondary);
	const usage = account.usageSummary;
	const hasUsage = Boolean(usage && (usage.lifetimeTokens != null || usage.peakDailyTokens != null || usage.longestRunningTurnSeconds != null || usage.currentStreakDays != null || usage.longestStreakDays != null));
	const resetCredits = account.capacity.resetCredits;
	const hasDetails = Boolean(plan || hasOverall || additionalBuckets.length > 0 || hasUsage || resetCredits);
	const capacityNotice = capacityNoticeFor(account, t, i18n.language);
	return (
		<div className="ml-9 mt-4 space-y-5 pb-1 text-xs">
			{capacityNotice ? <CapacityNotice {...capacityNotice} /> : null}
			{plan || resetCredits ? (
				<PlanCard plan={plan} resetCredits={resetCredits} resetEnabled={resetCreditSupported && !mutationDisabled} resetBusy={resetBusy} locale={i18n.language} onUseReset={onUseReset} />
			) : null}
			{hasUsage ? <AccountActivity usage={usage} locale={i18n.language} /> : null}
			{hasOverall && account.capacity.overall ? (
				<CapacityBucketGroup bucket={account.capacity.overall} title={t("settings.codexAccounts.generalUsageLimits")} locale={i18n.language} />
			) : null}
			{additionalBuckets.map((bucket) => (
				<CapacityBucketGroup
					key={bucket.limitId}
					bucket={bucket}
					title={bucket.displayName
						? t("settings.codexAccounts.namedUsageLimits", { name: bucket.displayName })
						: t("settings.codexAccounts.additionalUsageLimits")}
					locale={i18n.language}
				/>
			))}
			{!hasDetails && !capacityNotice ? <p className="text-muted-foreground">{t("settings.codexAccounts.usageDetailsUnavailable")}</p> : null}
		</div>
	);
}

type CapacityBucketValue = NonNullable<CodexAccount["capacity"]["overall"]>;
type CapacityWindowValue = NonNullable<CapacityBucketValue["primary"]>;
type UsageSummaryValue = NonNullable<CodexAccount["usageSummary"]>;
type ResetCreditsValue = NonNullable<CodexAccount["capacity"]["resetCredits"]>;

function CapacityBucketGroup({ bucket, title, locale }: { bucket: CapacityBucketValue; title: string; locale: string }) {
	const { t } = useTranslation();
	const windows = [bucket.primary, bucket.secondary].filter((window): window is CapacityWindowValue => Boolean(window));
	return (
		<section>
			<h4 className="mb-2 font-medium text-foreground">{title}</h4>
			<div className="divide-y divide-border/70 overflow-hidden rounded-md border border-border/70 bg-muted/15">
				{windows.map((window, index) => (
					<CapacityWindowRow
						key={`${index}-${window.windowDurationMinutes ?? "unknown"}-${window.resetsAt ?? "never"}`}
						window={window}
						label={capacityWindowLabel(window.windowDurationMinutes, index, windows.length, t)}
						reached={bucket.reached === "reached"}
						locale={locale}
					/>
				))}
			</div>
		</section>
	);
}

function CapacityWindowRow({ window, label, reached, locale }: { window: CapacityWindowValue; label: string; reached: boolean; locale: string }) {
	const { t } = useTranslation();
	const remaining = Math.max(0, Math.min(100, 100 - window.usedPercent));
	const percentage = formatPercentage(remaining, locale);
	const reset = formatResetTime(window.resetsAt, locale);
	const tone = reached || remaining <= 0 ? "exhausted" : remaining <= 25 ? "near" : "available";
	const fillClass = tone === "exhausted" ? "bg-error" : tone === "near" ? "bg-warning" : "bg-foreground/80";
	const valueClass = tone === "exhausted" ? "text-error" : tone === "near" ? "text-warning" : "text-muted-foreground";
	return (
		<div className="grid gap-2 px-3.5 py-3 sm:grid-cols-[minmax(0,1fr)_minmax(8rem,11rem)_auto] sm:items-center sm:gap-4">
			<div className="min-w-0">
				<p className="font-medium text-foreground">{label}</p>
				{reset ? <p className="mt-0.5 text-muted-foreground" title={reset.full}>{t("settings.codexAccounts.capacityResets", { value: reset.visible })}</p> : null}
			</div>
			<div
				role="progressbar"
				aria-label={t("settings.codexAccounts.remainingForLimit", { label, value: percentage })}
				aria-valuemin={0}
				aria-valuemax={100}
				aria-valuenow={remaining}
				className="h-1.5 w-full overflow-hidden rounded-full bg-muted"
			>
				<div className={`h-full rounded-full transition-[width] ${fillClass}`} style={{ width: `${remaining}%` }} />
			</div>
			<p className={`whitespace-nowrap text-right tabular-nums ${valueClass}`}>{t("settings.codexAccounts.percentLeft", { value: percentage })}</p>
		</div>
	);
}

function PlanCard({ plan, resetCredits, resetEnabled, resetBusy, locale, onUseReset }: { plan: string | null; resetCredits: ResetCreditsValue | null | undefined; resetEnabled: boolean; resetBusy: boolean; locale: string; onUseReset: () => void }) {
	const { t } = useTranslation();
	const expiry = formatResetTime(resetCredits?.nearestExpiresAt, locale);
	return (
		<section aria-labelledby="codex-account-plan-heading">
			<h4 id="codex-account-plan-heading" className="mb-2 font-medium text-foreground">{t("settings.codexAccounts.yourPlan")}</h4>
			<div className="flex flex-wrap items-center justify-between gap-4 rounded-md border border-border/70 bg-muted/15 px-3.5 py-3">
				{plan ? <p className="text-sm font-medium text-foreground">{plan}</p> : <span />}
				{resetCredits ? (
					<div className="flex items-center gap-3">
						<div className="text-right">
							<p className="font-medium text-foreground">
								{resetCredits.availableCount > 0
									? t("settings.codexAccounts.resetCount", { count: resetCredits.availableCount })
									: t("settings.codexAccounts.noResetsAvailable")}
							</p>
							{expiry ? <p className="mt-0.5 text-muted-foreground" title={expiry.full}>{t("settings.codexAccounts.resetExpires", { value: expiry.visible })}</p> : null}
						</div>
						{resetCredits.availableCount > 0 && resetEnabled ? (
							<Button type="button" size="sm" variant="outline" disabled={resetBusy} onClick={onUseReset}>
								{resetBusy ? <LoaderCircle className="animate-spin" aria-hidden="true" /> : <RotateCcw aria-hidden="true" />}
								{t("settings.codexAccounts.useReset")}
							</Button>
						) : null}
					</div>
				) : null}
			</div>
		</section>
	);
}

function AccountActivity({ usage, locale }: { usage: UsageSummaryValue | null | undefined; locale: string }) {
	const { t } = useTranslation();
	const metrics = [
		usage?.lifetimeTokens == null ? null : {
			label: t("settings.codexAccounts.lifetimeTokens"),
			value: t("settings.codexAccounts.tokenCount", { value: formatCompactNumber(usage.lifetimeTokens, locale) }),
		},
		usage?.peakDailyTokens == null ? null : {
			label: t("settings.codexAccounts.peakTokens"),
			value: t("settings.codexAccounts.tokenCount", { value: formatCompactNumber(usage.peakDailyTokens, locale) }),
		},
		usage?.longestRunningTurnSeconds == null ? null : {
			label: t("settings.codexAccounts.longestChat"),
			value: formatDuration(usage.longestRunningTurnSeconds, locale),
		},
		usage?.currentStreakDays == null ? null : {
			label: t("settings.codexAccounts.currentStreak"),
			value: t("settings.codexAccounts.dayCount", { count: usage.currentStreakDays }),
		},
		usage?.longestStreakDays == null ? null : {
			label: t("settings.codexAccounts.longestStreak"),
			value: t("settings.codexAccounts.dayCount", { count: usage.longestStreakDays }),
		},
	].filter((metric): metric is { label: string; value: string } => Boolean(metric));
	if (metrics.length === 0) return null;
	return (
		<section aria-labelledby="codex-account-activity-heading">
			<h4 id="codex-account-activity-heading" className="mb-2 font-medium text-foreground">{t("settings.codexAccounts.activity")}</h4>
			<div className="rounded-md border border-border/70 bg-muted/15">
				<div
					data-testid="codex-account-activity-metrics"
					className="grid divide-x divide-border/70"
					style={{ gridTemplateColumns: `repeat(${metrics.length}, minmax(0, 1fr))` }}
				>
					{metrics.map((metric) => (
						<div key={metric.label} className="min-w-0 px-2 py-2.5 text-center">
							<p className="truncate text-[13px] font-semibold leading-5 tabular-nums text-foreground" title={metric.value}>{metric.value}</p>
							<p className="mt-0.5 text-[10px] leading-tight text-muted-foreground">{metric.label}</p>
						</div>
					))}
				</div>
			</div>
		</section>
	);
}

function CapacityNotice({ reason, tone, checking }: { reason: string; tone: "warning" | "error" | "muted"; checking?: boolean }) {
	const color = tone === "error" ? "border-error/30 bg-error/8 text-error" : tone === "warning" ? "border-warning/30 bg-warning/10 text-warning" : "border-border bg-muted/20 text-muted-foreground";
	return (
		<p className={`flex items-start gap-2 rounded-md border px-3 py-2.5 leading-5 ${color}`} role={tone === "error" ? "alert" : "status"}>
			{checking ? <LoaderCircle className="mt-0.5 size-3.5 shrink-0 animate-spin" aria-hidden="true" /> : <CircleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />}
			<span>{reason}</span>
		</p>
	);
}

function capacityNoticeFor(account: CodexAccount, t: TFunction, locale: string): { reason: string; tone: "warning" | "error" | "muted"; checking?: boolean } | null {
	if (account.status === "broken") return { reason: account.reason, tone: "error" };
	if (account.authentication.state === "unauthorized") return { reason: account.authentication.reason, tone: "warning" };
	if (account.capacity.freshness === "checking") return { reason: t("settings.codexAccounts.capacityChecking"), tone: "muted", checking: true };
	if (account.capacity.freshness === "stale") {
		const checked = account.capacity.checkedAt ? formatObservedTime(account.capacity.checkedAt, locale) : null;
		return { reason: checked ? t("settings.codexAccounts.capacityStaleChecked", { value: checked }) : t("settings.codexAccounts.capacityStale"), tone: "warning" };
	}
	if (account.capacity.state === "unknown" || account.capacity.state === "unsupported") return { reason: account.capacity.reason, tone: "muted" };
	return null;
}

function capacityWindowLabel(minutes: number | null | undefined, index: number, count: number, t: TFunction): string {
	if (minutes === 300) return t("settings.codexAccounts.fiveHourUsageLimit");
	if (minutes === 10080) return t("settings.codexAccounts.weeklyUsageLimit");
	if (minutes && minutes > 0 && minutes % 1440 === 0) return t("settings.codexAccounts.dayUsageLimit", { count: minutes / 1440 });
	if (minutes && minutes > 0 && minutes % 60 === 0) return t("settings.codexAccounts.hourUsageLimit", { count: minutes / 60 });
	if (minutes && minutes > 0) return t("settings.codexAccounts.minuteUsageLimit", { count: minutes });
	if (count === 1) return t("settings.codexAccounts.usageLimit");
	return index === 0 ? t("settings.codexAccounts.primaryUsageLimit") : t("settings.codexAccounts.secondaryUsageLimit");
}

function formatAuthMethod(method: CodexAccount["authMethod"]): string | null {
	if (method === "chatgpt") return "ChatGPT";
	if (method === "api_key") return "API key";
	if (method === "other") return "Codex";
	return null;
}

function formatPlanName(plan: string | null | undefined): string | null {
	const value = plan?.trim();
	if (!value) return null;
	const known: Record<string, string> = { free: "Free", plus: "Plus", pro: "Pro", team: "Team", business: "Business", enterprise: "Enterprise" };
	return known[value.toLowerCase()] ?? value;
}

function formatPlanLabel(plan: string | null | undefined, t: TFunction): string | null {
	const name = formatPlanName(plan);
	if (!name) return null;
	return /\bplan$/i.test(name) ? name : t("settings.codexAccounts.planLabel", { name });
}

function formatPercentage(value: number, locale?: string): string {
	return `${new Intl.NumberFormat(locale, { maximumFractionDigits: 1 }).format(value)}%`;
}

function formatResetTime(value: string | null | undefined, locale: string): { visible: string; full: string } | null {
	if (!value) return null;
	const date = new Date(value);
	if (Number.isNaN(date.getTime())) return null;
	const now = new Date();
	const sameDay = date.getFullYear() === now.getFullYear() && date.getMonth() === now.getMonth() && date.getDate() === now.getDate();
	return {
		visible: new Intl.DateTimeFormat(locale, sameDay
			? { hour: "2-digit", minute: "2-digit" }
			: { day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" }).format(date),
		full: new Intl.DateTimeFormat(locale, { dateStyle: "full", timeStyle: "long" }).format(date),
	};
}

function formatObservedTime(value: string, locale: string): string {
	const date = new Date(value);
	if (Number.isNaN(date.getTime())) return "";
	return new Intl.DateTimeFormat(locale, { day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" }).format(date);
}

function formatCompactNumber(value: number, locale: string): string {
	return new Intl.NumberFormat(locale, { notation: "compact", maximumFractionDigits: 1 }).format(value);
}

function formatDuration(totalSeconds: number, locale: string): string {
	const seconds = Math.max(0, Math.round(totalSeconds));
	const hours = Math.floor(seconds / 3600);
	const minutes = Math.floor((seconds % 3600) / 60);
	const remainder = seconds % 60;
	const number = new Intl.NumberFormat(locale);
	if (hours > 0) return `${number.format(hours)}h ${number.format(minutes)}m`;
	if (minutes > 0) return `${number.format(minutes)}m`;
	return `${number.format(remainder)}s`;
}

function CodexAccountLoginTerminalPanel({ workflow, onCheckAgain, onClose, onRetry, onTerminalState }: {
	workflow: CodexAccountLoginTerminalWorkflow;
	onCheckAgain: () => void;
	onClose: () => void;
	onRetry: () => void;
	onTerminalState: (state: TerminalSessionState) => void;
}) {
	const { t } = useTranslation();
	const theme = useResolvedTheme();
	const shell = useShellMaybe();
	const panelRef = useRef<HTMLDivElement>(null);
	const terminalStateHandlerRef = useRef(onTerminalState);
	terminalStateHandlerRef.current = onTerminalState;
	const handleTerminalState = useCallback((state: TerminalSessionState) => terminalStateHandlerRef.current(state), []);
	useEffect(() => { panelRef.current?.scrollIntoView({ behavior: "smooth", block: "nearest" }); }, [workflow.terminal.handleId]);
	const status = workflow.phase === "running" ? t("settings.codexAccounts.loginRunning") : workflow.phase === "verifying" ? t("settings.codexAccounts.loginVerifying") : workflow.phase === "closing" ? t("settings.codexAccounts.loginClosing") : (workflow.reason ?? t("settings.codexAccounts.loginUnverified"));
	const retryable = workflow.phase === "unauthorized" || workflow.phase === "expired" || workflow.phase === "failed";
	const checkable = workflow.phase === "unverified";
	return (
		<div ref={panelRef} className="scroll-my-3 overflow-hidden rounded-md border border-border bg-terminal" data-testid="codex-account-login-terminal">
			<div className="flex min-h-10 items-center justify-between gap-3 border-b border-border bg-surface/90 px-3 py-2"><div className="min-w-0"><p className="truncate text-xs font-medium text-foreground">{t("settings.codexAccounts.loginTerminalTitle")}</p><p className="truncate text-[11px] text-muted-foreground" aria-live="polite" role="status">{status}</p></div><button type="button" aria-label={t("settings.codexAccounts.loginClose")} className="grid size-7 shrink-0 place-items-center rounded text-muted-foreground hover:bg-interactive-hover hover:text-foreground disabled:opacity-50" disabled={workflow.phase === "closing"} onClick={onClose}><X className="size-4" aria-hidden="true" /></button></div>
			<div className="h-[300px] min-h-0"><TerminalPane daemonReady={shell ? shell.daemonStatus.state === "ready" : true} fontSize={12} onTerminalStateChange={handleTerminalState} terminalTarget={{ kind: "shell", handleId: workflow.terminal.handleId, generation: workflow.terminal.createdAt, title: workflow.terminal.title }} theme={theme} /></div>
			{workflow.reason || retryable || checkable ? <div className="flex items-center justify-between gap-3 border-t border-border bg-surface/90 px-3 py-2"><p className="min-w-0 text-xs text-muted-foreground" role={workflow.reason ? "alert" : undefined}>{workflow.reason}</p><div className="flex shrink-0 items-center gap-2">{retryable ? <Button type="button" size="sm" variant="outline" onClick={onRetry}>{t("settings.codexAccounts.retry")}</Button> : null}{checkable ? <Button type="button" size="sm" variant="outline" onClick={onCheckAgain}>{t("settings.codexAccounts.loginCheckAgain")}</Button> : null}<Button type="button" size="sm" variant="ghost" onClick={onClose}>{t("settings.codexAccounts.loginClose")}</Button></div></div> : null}
		</div>
	);
}
