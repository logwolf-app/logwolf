import { Plus, Trash2, TriangleAlert } from 'lucide-react';
import { useEffect, useRef } from 'react';
import { useFetcher } from 'react-router';

import { type SettingsActionResult, useSuccessToast } from '~/components/settings/action-result';
import { SettingsRow } from '~/components/settings/settings-row';
import { Alert, AlertDescription, AlertTitle } from '~/components/ui/alert';
import { Badge } from '~/components/ui/badge';
import { Button } from '~/components/ui/button';
import { Card, CardContent } from '~/components/ui/card';
import { Field, FieldGroup, FieldLabel } from '~/components/ui/field';
import { Input } from '~/components/ui/input';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '~/components/ui/select';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '~/components/ui/table';
import { useCsrfToken } from '~/hooks/use-csrf-token';
import type { OrganizationMember, OrganizationRole } from '~/lib/api';
import { assignableOrganizationRoles, canManageOrganization } from '~/lib/organizations';

type Props = {
	members: OrganizationMember[];
	currentUser: { id: number; login: string };
	/** The signed-in user's role in the organization. */
	callerRole: OrganizationRole;
};

export function MembersSection({ members, currentUser, callerRole }: Props) {
	const csrfToken = useCsrfToken();
	const canManage = canManageOrganization(callerRole);
	const roles = assignableOrganizationRoles(callerRole);

	// As on the project page: one fetcher per kind of change, so each reports
	// its own error and a pending removal doesn't grey out the add form.
	const addFetcher = useFetcher<SettingsActionResult>();
	const roleFetcher = useFetcher<SettingsActionResult>();
	const removeFetcher = useFetcher<SettingsActionResult>();

	useSuccessToast(addFetcher.data);
	useSuccessToast(roleFetcher.data);
	useSuccessToast(removeFetcher.data);

	const addFormRef = useRef<HTMLFormElement>(null);

	useEffect(() => {
		if (addFetcher.data?.success) addFormRef.current?.reset();
	}, [addFetcher.data]);

	// An organization always keeps one owner, and only owners change owners.
	// The broker refuses both; leaving the controls out only saves the round trip.
	const ownerCount = members.filter((m) => m.role === 'owner').length;
	const canChange = (member: OrganizationMember) =>
		canManage && !(member.role === 'owner' && ownerCount === 1) && (member.role !== 'owner' || callerRole === 'owner');

	const removing = removeFetcher.formData?.get('member')?.toString();
	const changingRole = roleFetcher.formData?.get('member')?.toString();
	const pendingRole = roleFetcher.formData?.get('role')?.toString();

	function changeRole(member: OrganizationMember, role: string) {
		roleFetcher.submit(
			{ _csrf: csrfToken, intent: 'change-role', member: member.id, login: member.github_login, role },
			{ method: 'post' },
		);
	}

	const error = addFetcher.data?.error ?? roleFetcher.data?.error ?? removeFetcher.data?.error;
	const warning = addFetcher.data?.warning;

	return (
		<SettingsRow
			title='Members'
			description='Owners own every project in the organization. Owners and admins rename it and manage its members; only owners add, remove or change owners.'
		>
			<div className='flex flex-col gap-3'>
				{error && (
					<Alert variant='destructive'>
						<AlertTitle>{error}</AlertTitle>
					</Alert>
				)}

				{warning && (
					<Alert variant='warning'>
						<TriangleAlert />
						<AlertTitle>They may not be able to sign in</AlertTitle>
						<AlertDescription>{warning}</AlertDescription>
					</Alert>
				)}

				<div className='overflow-hidden rounded-lg border bg-card'>
					<Table>
						<TableHeader>
							<TableRow>
								<TableHead>Member</TableHead>
								<TableHead>Role</TableHead>
								<TableHead>Joined</TableHead>
								{canManage && <TableHead className='w-0' />}
							</TableRow>
						</TableHeader>

						<TableBody>
							{members.map((member) => {
								const editable = canChange(member);
								const isLastOwner = member.role === 'owner' && ownerCount === 1;

								return (
									<TableRow key={member.id}>
										<TableCell>
											<div className='flex items-center gap-2.5'>
												<span
													aria-hidden
													className='flex size-7 shrink-0 items-center justify-center rounded-md border bg-muted text-xs font-medium text-muted-foreground uppercase'
												>
													{member.github_login.slice(0, 2)}
												</span>
												<span className='font-medium'>{member.github_login}</span>
												{member.user_id === currentUser.id && <Badge variant='outline'>you</Badge>}
											</div>
										</TableCell>

										<TableCell>
											{editable ? (
												<Select
													value={changingRole === member.id ? pendingRole : member.role}
													onValueChange={(role) => changeRole(member, role)}
													disabled={changingRole === member.id}
												>
													<SelectTrigger size='sm' className='w-28' aria-label={`Role of ${member.github_login}`}>
														<SelectValue />
													</SelectTrigger>

													<SelectContent>
														{roles.map((role) => (
															<SelectItem key={role} value={role}>
																{role}
															</SelectItem>
														))}
													</SelectContent>
												</Select>
											) : (
												<Badge variant={member.role === 'member' ? 'secondary' : 'default'}>{member.role}</Badge>
											)}
										</TableCell>

										<TableCell className='text-muted-foreground'>
											{new Date(member.created_at).toLocaleDateString()}
										</TableCell>

										{canManage && (
											<TableCell>
												{isLastOwner ? (
													<span className='text-xs whitespace-nowrap text-muted-foreground'>Last owner</span>
												) : (
													editable && (
														<removeFetcher.Form method='post'>
															<input type='hidden' name='_csrf' value={csrfToken} />
															<input type='hidden' name='intent' value='remove-member' />
															<input type='hidden' name='member' value={member.id} />
															<input type='hidden' name='login' value={member.github_login} />

															<Button
																type='submit'
																variant='ghost'
																size='icon-sm'
																aria-label={`Remove ${member.github_login}`}
																disabled={removing === member.id}
															>
																<Trash2 />
															</Button>
														</removeFetcher.Form>
													)
												)}
											</TableCell>
										)}
									</TableRow>
								);
							})}
						</TableBody>
					</Table>
				</div>

				{canManage && (
					<Card className='py-4'>
						<CardContent className='px-4'>
							<addFetcher.Form method='post' ref={addFormRef}>
								<FieldGroup>
									<input type='hidden' name='_csrf' value={csrfToken} />
									<input type='hidden' name='intent' value='add-member' />

									<div className='flex flex-col gap-3 sm:flex-row sm:items-end'>
										<Field className='flex-1'>
											<FieldLabel htmlFor='login'>GitHub login</FieldLabel>
											<Input id='login' name='login' type='text' placeholder='octocat' required />
										</Field>

										<Field className='sm:w-32'>
											<FieldLabel htmlFor='role'>Role</FieldLabel>

											<Select name='role' defaultValue='member'>
												<SelectTrigger id='role' className='w-full'>
													<SelectValue placeholder='Role' />
												</SelectTrigger>

												<SelectContent>
													{roles.map((role) => (
														<SelectItem key={role} value={role}>
															{role}
														</SelectItem>
													))}
												</SelectContent>
											</Select>
										</Field>

										<Button type='submit' disabled={addFetcher.state !== 'idle'}>
											<Plus />
											Add member
										</Button>
									</div>
								</FieldGroup>
							</addFetcher.Form>
						</CardContent>
					</Card>
				)}
			</div>
		</SettingsRow>
	);
}
