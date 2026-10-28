import { Outlet, Link, useLocation, useLoaderData } from 'react-router';
import {
  Home,
  Database,
  Archive,
  Calendar,
  Shield,
  Users,
} from 'lucide-react';
import {
  Sidebar,
  SidebarContent,
  SidebarGroup,
  SidebarGroupLabel,
  SidebarMenu,
  SidebarMenuItem,
  SidebarMenuButton,
  SidebarProvider,
  SidebarHeader,
  SidebarFooter,
} from '../ui/sidebar';
import { AuthProvider } from '../../lib/contexts/auth-context';
import { WebSocketProvider } from '../../lib/contexts/websocket-context';
import { DockerProvider } from '../../lib/contexts/docker-context';
import { ROUTES } from '../../lib/constants';
import { UserMenu } from './user-menu';
import { ThemeToggle } from './theme-toggle';
import { DockerUnavailableBanner } from '../features/docker-banner';
import type { User } from '../../lib/types';

interface NavItem {
  title: string;
  href: string;
  icon: typeof Home;
  adminOnly?: boolean;
}

const navItems: NavItem[] = [
  {
    title: 'Dashboard',
    href: ROUTES.DASHBOARD,
    icon: Home,
  },
  {
    title: 'Volumes',
    href: ROUTES.VOLUMES,
    icon: Database,
  },
  {
    title: 'Snapshots',
    href: ROUTES.SNAPSHOTS,
    icon: Archive,
  },
  {
    title: 'Jobs',
    href: ROUTES.JOBS,
    icon: Calendar,
  },
  {
    title: 'Policies',
    href: ROUTES.POLICIES,
    icon: Shield,
  },
  {
    title: 'Users',
    href: ROUTES.USERS,
    icon: Users,
    adminOnly: true,
  },
];

export function RootLayout() {
  const loaderData = useLoaderData() as { user: User } | null;
  const location = useLocation();

  // Safeguard: if no user data, this shouldn't render (loader should redirect)
  // But we handle it gracefully anyway
  const user = loaderData?.user;

  return (
    <AuthProvider initialUser={user || null}>
      <WebSocketProvider isAuthenticated={!!user}>
        <DockerProvider>
          <RootLayoutContent user={user} location={location} />
        </DockerProvider>
      </WebSocketProvider>
    </AuthProvider>
  );
}

function RootLayoutContent({ user, location }: { user: User | null | undefined; location: any }) {
  const visibleNavItems = navItems.filter(
    (item) => {
      // Filter out admin-only items
      if (item.adminOnly && user?.role !== 'admin') {
        return false;
      }
      return true;
    }
  );

  return (
    <SidebarProvider>
      <div className="flex h-screen w-full bg-background">
        <Sidebar className="border-r">
          <SidebarHeader className="border-b px-6 py-5">
            <div className="flex items-center gap-2">
              <div className="flex h-8 w-8 items-center justify-center rounded-md bg-primary text-primary-foreground">
                <Archive className="h-4 w-4" />
              </div>
              <h1 className="text-lg font-semibold tracking-tight">arkUI</h1>
            </div>
          </SidebarHeader>
          <SidebarContent className="px-3 py-4">
            <SidebarGroup>
              <SidebarGroupLabel className="px-3 text-xs font-medium text-muted-foreground">
                Navigation
              </SidebarGroupLabel>
              <SidebarMenu className="gap-1">
                {visibleNavItems.map((item) => {
                  const isActive = location.pathname === item.href || 
                    (item.href !== ROUTES.DASHBOARD && location.pathname.startsWith(item.href));
                  
                  return (
                    <SidebarMenuItem key={item.href}>
                      <SidebarMenuButton asChild isActive={isActive}>
                        <Link to={item.href} className="gap-3">
                          <item.icon className="h-4 w-4" />
                          <span className="text-sm font-medium">{item.title}</span>
                        </Link>
                      </SidebarMenuButton>
                    </SidebarMenuItem>
                  );
                })}
              </SidebarMenu>
            </SidebarGroup>
          </SidebarContent>
          <SidebarFooter className="border-t px-4 py-3">
            <div className="flex items-center justify-between gap-2">
              <UserMenu />
              <ThemeToggle />
            </div>
          </SidebarFooter>
        </Sidebar>
        
        <main className="flex min-h-screen flex-1 flex-col overflow-hidden">
          <div className="flex-1 overflow-auto p-6">
            <DockerUnavailableBanner />
            <Outlet />
          </div>
        </main>
      </div>
    </SidebarProvider>
  );
}
