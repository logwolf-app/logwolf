/**
 * Whether moving a project from `from` days of retention to `to` shortens it.
 * 0 is forever, the longest of all.
 *
 * Mirrors the broker's `data.LowersRetention`: any member may keep logs longer,
 * but lowering retention makes the next cleanup pass delete everything outside
 * the new window, so only an owner may.
 */
export function lowersRetention(from: number, to: number): boolean {
	if (to === 0) return false;
	return from === 0 || to < from;
}

/** How the dashboard names a retention of `days` days. */
export function retentionLabel(days: number): string {
	return days === 0 ? 'Forever' : `${days} days`;
}

/**
 * The values to list for a project's retention: the `choices` the broker gave
 * it, plus `current` when that is not one of them (a plan that no longer offers
 * it), so the control still shows what is set. Only `choices` can be saved.
 */
export function retentionOptions(current: number, choices: readonly number[]): number[] {
	return choices.includes(current) ? [...choices] : [current, ...choices];
}
