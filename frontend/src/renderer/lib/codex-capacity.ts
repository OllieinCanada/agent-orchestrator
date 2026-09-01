import type { components } from "../../api/schema";

export type CodexCapacity = components["schemas"]["CodexCapacitySnapshot"];
export type CodexCapacityState = CodexCapacity["state"];

export function codexCapacityTranslationKey(state: CodexCapacityState) {
	switch (state) {
		case "available":
			return "settings.codexAccounts.capacityAvailable" as const;
		case "near_limit":
			return "settings.codexAccounts.capacityNearLimit" as const;
		case "exhausted":
			return "settings.codexAccounts.capacityExhausted" as const;
		case "unsupported":
			return "settings.codexAccounts.capacityUnsupported" as const;
		default:
			return "settings.codexAccounts.capacityUnknown" as const;
	}
}

export function codexCapacitySummary(capacity: CodexCapacity, stateLabel: string): string {
	const remaining = capacity.remainingPercent === undefined || capacity.remainingPercent === null
		? undefined
		: `${capacity.remainingPercent}%`;
	return [capacity.plan, remaining, stateLabel].filter(Boolean).join(" · ");
}
