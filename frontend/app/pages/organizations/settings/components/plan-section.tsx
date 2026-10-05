import { SettingsRow } from '~/components/settings/settings-row';
import { Badge } from '~/components/ui/badge';
import { Card } from '~/components/ui/card';
import type { OrganizationPlan } from '~/lib/api';
import { locale } from '~/lib/locale';
import { retentionLabel } from '~/lib/retention';
import { cn } from '~/lib/utils';

type Props = { plan: OrganizationPlan };

/** The plan's limits, and how much of each the organization uses where it is counted. */
export function PlanSection({ plan: { plan, usage } }: Props) {
	return (
		<SettingsRow
			title='Plan and usage'
			description='The plan sets the limits every project in this organization works within.'
		>
			<Card className='gap-0 overflow-hidden py-0'>
				<div className='flex flex-col gap-0.5 border-b p-5'>
					<span className='text-xs text-muted-foreground'>Current plan</span>
					<span className='font-medium capitalize'>{plan.name}</span>
				</div>

				<dl className='divide-y'>
					<UsageRow label='Projects' used={usage.projects} limit={plan.max_projects} />
					<UsageRow label='Members' used={usage.members} limit={plan.max_members} />
					<UsageRow label='Events per month' limit={plan.monthly_events} />
					<LimitRow label='Longest retention' value={retentionLabel(plan.max_retention_days)} />
				</dl>
			</Card>
		</SettingsRow>
	);
}

const formatCount = (n: number) => n.toLocaleString(locale);

/**
 * One limit with what is used of it. A limit of 0 is none; `used` is left out
 * where it is not counted yet.
 */
function UsageRow({ label, used, limit }: { label: string; used?: number; limit: number }) {
	const unlimited = limit === 0;
	const share = used === undefined || unlimited ? undefined : Math.min(used / limit, 1);

	return (
		<div className='flex flex-col gap-2 px-5 py-3'>
			<div className='flex items-center justify-between gap-4 text-sm'>
				<dt className='text-muted-foreground'>{label}</dt>
				<dd className='flex items-center gap-2 tabular-nums'>
					{used !== undefined && <span className='font-medium'>{formatCount(used)}</span>}
					{used !== undefined && <span className='text-muted-foreground'>of</span>}
					{unlimited ? <Badge variant='secondary'>Unlimited</Badge> : <span>{formatCount(limit)}</span>}
				</dd>
			</div>

			{share !== undefined && (
				<div
					role='meter'
					aria-label={`${label} used`}
					aria-valuemin={0}
					aria-valuemax={limit}
					aria-valuenow={used}
					className='h-1.5 overflow-hidden rounded-full bg-muted'
				>
					<div
						className={cn('h-full rounded-full bg-primary', share >= 1 && 'bg-destructive')}
						style={{ width: `${share * 100}%` }}
					/>
				</div>
			)}
		</div>
	);
}

function LimitRow({ label, value }: { label: string; value: string }) {
	return (
		<div className='flex items-center justify-between gap-4 px-5 py-3 text-sm'>
			<dt className='text-muted-foreground'>{label}</dt>
			<dd>{value}</dd>
		</div>
	);
}
