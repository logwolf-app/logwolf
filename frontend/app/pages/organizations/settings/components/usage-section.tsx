import { SettingsRow } from '~/components/settings/settings-row';
import { Card } from '~/components/ui/card';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '~/components/ui/table';
import type { OrganizationPlan, OrganizationProjectsUsage, ProjectUsage } from '~/lib/api';
import { formatPercent } from '~/lib/format';
import { locale } from '~/lib/locale';
import { formatBytes, formatUsageMonth, quotaShare, wasMeasured } from '~/lib/usage';
import { cn } from '~/lib/utils';

type Props = {
	/** Null for a member: only owners and admins may see every project's usage. */
	usage: OrganizationProjectsUsage | null;
	plan: OrganizationPlan['plan'];
};

/**
 * What each of the organization's projects ingested this month, against the
 * plan's monthly events, and what each stores now.
 */
export function UsageSection({ usage, plan }: Props) {
	if (!usage) {
		return (
			<SettingsRow title='Usage by project' description='Events and storage of each project in this organization.'>
				<p className='text-sm text-muted-foreground'>Only owners and admins of this organization can see it.</p>
			</SettingsRow>
		);
	}

	const month = formatUsageMonth(usage.month, locale);
	const deleted = usage.deleted.events > 0 || usage.deleted.bytes > 0;

	return (
		<SettingsRow
			title='Usage by project'
			description={
				<>
					Events each project ingested in {month} (UTC)
					{plan.monthly_events > 0 && <>, against the plan’s {formatCount(plan.monthly_events)} a month</>}, and what it
					stores now.
				</>
			}
		>
			<Card className='gap-0 overflow-hidden py-0'>
				{usage.projects.length === 0 && !deleted ? (
					<p className='p-5 text-sm text-muted-foreground'>This organization has no projects yet.</p>
				) : (
					<Table>
						<TableHeader>
							<TableRow>
								<TableHead className='pl-5'>Project</TableHead>
								<TableHead className='text-right'>Events</TableHead>
								<TableHead className='text-right'>Ingested</TableHead>
								<TableHead className='pr-5 text-right'>Stored</TableHead>
							</TableRow>
						</TableHeader>
						<TableBody>
							{usage.projects.map((p) => (
								<ProjectRow key={p.project_id} project={p} monthlyEvents={plan.monthly_events} />
							))}
							{deleted && (
								<TableRow>
									<TableCell className='pl-5 text-muted-foreground'>Deleted projects</TableCell>
									<EventsCell events={usage.deleted.events} monthlyEvents={plan.monthly_events} />
									<TableCell className='text-right tabular-nums'>{formatBytes(usage.deleted.bytes, locale)}</TableCell>
									<TableCell className='pr-5 text-right text-muted-foreground'>—</TableCell>
								</TableRow>
							)}
						</TableBody>
					</Table>
				)}
			</Card>
			<p className='text-xs text-muted-foreground'>
				Events count once the broker accepts them, a minute behind at most. Storage is as of its last measure.
			</p>
		</SettingsRow>
	);
}

const formatCount = (n: number) => n.toLocaleString(locale);

/** In UTC, like the month, so the server and the browser word it alike. */
const formatMeasuredAt = (at: string) =>
	`${new Date(at).toLocaleString(locale, { dateStyle: 'medium', timeStyle: 'short', timeZone: 'UTC' })} UTC`;

function ProjectRow({ project, monthlyEvents }: { project: ProjectUsage; monthlyEvents: number }) {
	const measured = wasMeasured(project.storage_measured_at);

	return (
		<TableRow>
			<TableCell className='max-w-60 truncate pl-5 font-medium'>{project.name}</TableCell>
			<EventsCell events={project.events} monthlyEvents={monthlyEvents} />
			<TableCell className='text-right tabular-nums'>{formatBytes(project.bytes, locale)}</TableCell>
			<TableCell
				className='pr-5 text-right tabular-nums'
				title={measured ? `Measured ${formatMeasuredAt(project.storage_measured_at)}` : undefined}
			>
				{measured ? (
					<div className='flex flex-col items-end'>
						<span>{formatBytes(project.storage.bytes, locale)}</span>
						<span className='text-xs text-muted-foreground'>{formatCount(project.storage.events)} events</span>
					</div>
				) : (
					<span className='text-muted-foreground'>Not measured yet</span>
				)}
			</TableCell>
		</TableRow>
	);
}

/** A project's events this month, with its share of the plan's monthly events when the plan sets them. */
function EventsCell({ events, monthlyEvents }: { events: number; monthlyEvents: number }) {
	const share = quotaShare(events, monthlyEvents);

	return (
		<TableCell className='text-right tabular-nums'>
			<div className='flex flex-col items-end gap-1'>
				<span>{formatCount(events)}</span>
				{share !== undefined && (
					<div className='flex items-center gap-2'>
						<div
							role='meter'
							aria-label='Share of the monthly events'
							aria-valuemin={0}
							aria-valuemax={monthlyEvents}
							aria-valuenow={Math.min(events, monthlyEvents)}
							className='h-1 w-16 overflow-hidden rounded-full bg-muted'
						>
							<div
								className={cn('h-full rounded-full bg-primary', share >= 1 && 'bg-destructive')}
								style={{ width: `${Math.min(share, 1) * 100}%` }}
							/>
						</div>
						<span className='text-xs text-muted-foreground'>{formatPercent(share)}</span>
					</div>
				)}
			</div>
		</TableCell>
	);
}
