import { Building2, Check, ChevronsUpDown, Settings } from 'lucide-react';
import { Link, useLocation, useSubmit } from 'react-router';

import type { UserOrganization } from '~/lib/api';
import { pathAfterOrganizationSwitch } from '~/lib/organizations';

import {
	DropdownMenu,
	DropdownMenuContent,
	DropdownMenuItem,
	DropdownMenuLabel,
	DropdownMenuSeparator,
	DropdownMenuTrigger,
} from '../ui/dropdown-menu';
import { SidebarMenu, SidebarMenuButton, SidebarMenuItem, useSidebar } from '../ui/sidebar';

type Props = {
	organizations: UserOrganization[];
	currentOrganization: UserOrganization | undefined;
	csrfToken: string;
};

/**
 * Picks the organization the dashboard works in, above the project switcher,
 * whose projects follow it. Renders nothing for a user in no organization.
 */
export function OrganizationSwitcher({ organizations, currentOrganization, csrfToken }: Props) {
	const { isMobile } = useSidebar();
	const submit = useSubmit();
	const location = useLocation();

	if (!currentOrganization) return null;

	function switchTo(organizationId: string) {
		if (organizationId === currentOrganization?.id) return;

		// Like the project switcher: the action writes the session and sends us
		// on, which revalidates every loader against the new organization.
		submit(
			{ organizationId, redirectTo: pathAfterOrganizationSwitch(location.pathname, organizationId), _csrf: csrfToken },
			{ method: 'POST', action: '/organizations/switch' },
		);
	}

	return (
		<SidebarMenu>
			<SidebarMenuItem>
				<DropdownMenu>
					<DropdownMenuTrigger asChild>
						<SidebarMenuButton
							tooltip={currentOrganization.name}
							className='data-[state=open]:bg-sidebar-accent'
							aria-label={`Organization: ${currentOrganization.name}`}
						>
							<Building2 className='text-muted-foreground' />
							<span className='truncate text-xs font-medium'>{currentOrganization.name}</span>
							<ChevronsUpDown className='ml-auto text-muted-foreground' />
						</SidebarMenuButton>
					</DropdownMenuTrigger>

					<DropdownMenuContent
						className='w-(--radix-dropdown-menu-trigger-width) min-w-56'
						align='start'
						side={isMobile ? 'bottom' : 'right'}
					>
						<DropdownMenuLabel className='text-xs text-muted-foreground'>Organizations</DropdownMenuLabel>

						{organizations.map((organization) => (
							<DropdownMenuItem key={organization.id} onSelect={() => switchTo(organization.id)}>
								<Building2 />
								<span className='truncate'>{organization.name}</span>
								{organization.id === currentOrganization.id && <Check className='ml-auto' />}
							</DropdownMenuItem>
						))}

						<DropdownMenuSeparator />

						<DropdownMenuItem asChild>
							<Link to={`/organizations/${currentOrganization.id}/settings`}>
								<Settings />
								<span>Organization settings</span>
							</Link>
						</DropdownMenuItem>
					</DropdownMenuContent>
				</DropdownMenu>
			</SidebarMenuItem>
		</SidebarMenu>
	);
}
