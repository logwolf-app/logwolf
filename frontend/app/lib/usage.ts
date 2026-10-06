const BYTE_UNITS = ['B', 'kB', 'MB', 'GB', 'TB', 'PB'] as const;

/** A size in bytes in the largest decimal unit it reaches: "512 B", "1.5 kB", "20 MB". */
export function formatBytes(bytes: number, locale: string): string {
	let value = bytes;
	let unit = 0;
	while (value >= 1000 && unit < BYTE_UNITS.length - 1) {
		value /= 1000;
		unit++;
	}
	const digits = value < 10 && unit > 0 ? 1 : 0;
	return `${value.toLocaleString(locale, { maximumFractionDigits: digits })} ${BYTE_UNITS[unit]}`;
}

/**
 * Whether the logger has measured a project's storage: the broker sends Go's
 * zero time, the year 1, until it has.
 */
export function wasMeasured(measuredAt: string): boolean {
	const at = new Date(measuredAt);
	return !Number.isNaN(at.getTime()) && at.getUTCFullYear() > 1;
}

/**
 * The share of a plan's monthly events that `events` takes, from 0 to 1, past
 * 1 when a project alone went over; undefined for a plan without a monthly
 * limit (0).
 */
export function quotaShare(events: number, monthlyEvents: number): number | undefined {
	return monthlyEvents > 0 ? events / monthlyEvents : undefined;
}

/** The month a usage report covers, as the dashboard words it: "October 2026". */
export function formatUsageMonth(month: string, locale: string): string {
	return new Date(month).toLocaleDateString(locale, { month: 'long', year: 'numeric', timeZone: 'UTC' });
}
