import { Link } from 'react-router';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Button } from '@/components/ui/button';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import { Skeleton } from '@/components/ui/skeleton';
import { StatusBadge } from '@/components/features/status-badge';
import { EmptyState } from '@/components/features/empty-state';
import { Container } from '@/components/layout/container';
import { PageHeader } from '@/components/layout/page-header';
import { HardDrive, Archive, Clock, Activity, FileCheck } from 'lucide-react';
import { ROUTES } from '@/lib/constants';
import { formatBytes } from '@/lib/utils';
import { format, formatDistanceToNow } from 'date-fns';
import { useDocker } from '@/lib/contexts/docker-context';
import { api } from '@/lib/api';
import { useAsyncData } from '@/hooks/use-async-data';
import type { EngineStatus } from '@/lib/types';

const defaultStatus: EngineStatus = {
	status: 'unknown',
	version: '',
	repo_dir: '',
};

export function Dashboard() {
	const { dockerAvailable } = useDocker();

	const { data: status, loading: statusLoading } = useAsyncData(
() => api.getStatus(),
		defaultStatus
	);
	const { data: volumes, loading: volumesLoading } = useAsyncData(
() => api.listVolumes(),
		[]
	);
	const { data: snapshots, loading: snapshotsLoading } = useAsyncData(
() => api.listSnapshots(),
		[]
	);
	const { data: recentHistory, loading: historyLoading } = useAsyncData(
() => api.listRecentHistory(10),
		[]
	);

	const totalSize = snapshots.reduce((sum, s) => sum + s.size_bytes, 0);

	return (
<Container>
			<PageHeader
				title="Dashboard"
				description="Overview of your backup system and recent activity"
			/>

			<div className="space-y-6">
				{/* Status Card */}
				<Card>
					<CardContent className="p-4">
						<div className="flex items-center justify-between">
							<div className="flex items-center gap-3">
								<div className="h-10 w-10 rounded-lg bg-primary/10 flex items-center justify-center">
									<Activity className="h-5 w-5 text-primary" />
								</div>
								<div>
									<p className="text-sm font-medium">Ark Engine</p>
									{statusLoading ? (
<Skeleton className="h-4 w-24 mt-1" />
									) : (
<div className="flex items-center gap-2 mt-0.5">
											<StatusBadge status={status.status === 'ok' ? 'success' : 'failed'} />
											<span className="text-xs text-muted-foreground">{status.version}</span>
										</div>
									)}
								</div>
							</div>
						</div>
					</CardContent>
				</Card>

				{/* Stats Cards */}
				<div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
					<StatCard
						label="Volumes"
						value={volumes.length}
						loading={volumesLoading}
						icon={<HardDrive className="h-5 w-5 text-blue-600" />}
						iconBg="bg-blue-500/10"
						link={{ to: ROUTES.VOLUMES, label: 'View all volumes →' }}
					/>
					<StatCard
						label="Snapshots"
						value={snapshots.length}
						loading={snapshotsLoading}
						icon={<Archive className="h-5 w-5 text-green-600" />}
						iconBg="bg-green-500/10"
						link={{ to: ROUTES.SNAPSHOTS, label: 'View all snapshots →' }}
					/>
					<StatCard
						label="Total Size"
						value={formatBytes(totalSize)}
						loading={snapshotsLoading}
						icon={<FileCheck className="h-5 w-5 text-purple-600" />}
						iconBg="bg-purple-500/10"
						description="Across all snapshots"
					/>
					<StatCard
						label="Recent Jobs"
						value={recentHistory.length}
						loading={historyLoading}
						icon={<Clock className="h-5 w-5 text-amber-600" />}
						iconBg="bg-amber-500/10"
						description="Last 10 executions"
					/>
				</div>

				{/* Recent Activity */}
				<Card>
					<CardHeader className="border-b">
						<div className="flex items-center justify-between">
							<div>
								<CardTitle>Recent Activity</CardTitle>
								<CardDescription className="mt-1">
									Latest backup job executions
								</CardDescription>
							</div>
							<Button asChild variant="outline" size="sm">
								<Link to={ROUTES.JOBS}>View all</Link>
							</Button>
						</div>
					</CardHeader>
					<CardContent className="p-0">
						{historyLoading ? (
<div className="p-6 space-y-3">
								{[...Array(3)].map((_, i) => (
<div key={i} className="flex items-center gap-4">
										<Skeleton className="h-4 w-[30%]" />
										<Skeleton className="h-4 w-[15%]" />
										<Skeleton className="h-4 w-[20%]" />
										<Skeleton className="h-4 w-[10%]" />
										<Skeleton className="h-4 w-[15%]" />
									</div>
								))}
							</div>
						) : recentHistory.length === 0 ? (
<div className="flex items-center justify-center py-12">
								<EmptyState
									icon={<Clock className="h-12 w-12" />}
									title="No recent activity"
									description="Backup job executions will appear here once they run."
									action={{
										label: 'View Jobs',
										onClick: () => (window.location.href = ROUTES.JOBS)
									}}
								/>
							</div>
						) : (
<Table>
								<TableHeader>
									<TableRow>
										<TableHead className="w-[30%]">Job</TableHead>
										<TableHead>Status</TableHead>
										<TableHead>Started</TableHead>
										<TableHead>Duration</TableHead>
										<TableHead>Snapshot</TableHead>
									</TableRow>
								</TableHeader>
								<TableBody>
									{recentHistory.map((history) => {
										const duration = history.completed_at
											? `${Math.round(
(new Date(history.completed_at).getTime() -
new Date(history.started_at).getTime()) /
1000
)}s`
											: null;

										return (
<TableRow key={history.id} className="group">
												<TableCell>
													<div className="flex flex-col gap-1">
														<span className="text-sm font-medium">
															{history.job?.name || `Job #${history.job_id}`}
														</span>
														<span className="text-xs text-muted-foreground">
															ID: {history.job_id}
														</span>
													</div>
												</TableCell>
												<TableCell>
													<StatusBadge status={history.status} />
												</TableCell>
												<TableCell>
													<div className="flex flex-col gap-0.5">
														<span className="text-sm">
															{format(new Date(history.started_at), 'MMM d, HH:mm')}
														</span>
														<span className="text-xs text-muted-foreground">
															{formatDistanceToNow(new Date(history.started_at), {
addSuffix: true
})}
														</span>
													</div>
												</TableCell>
												<TableCell className="text-sm">{duration ?? '-'}</TableCell>
												<TableCell>
													{history.snapshot_id ? (
<code className="text-xs bg-muted px-1.5 py-0.5 rounded">
															{history.snapshot_id}
														</code>
													) : (
<span className="text-sm text-muted-foreground">-</span>
													)}
												</TableCell>
											</TableRow>
										);
									})}
								</TableBody>
							</Table>
						)}
					</CardContent>
				</Card>

				{/* Quick Actions */}
				<Card>
					<CardHeader>
						<CardTitle>Quick Actions</CardTitle>
						<CardDescription className="mt-1">Common tasks and shortcuts</CardDescription>
					</CardHeader>
					<CardContent>
						<div className="flex flex-wrap gap-2">
							{dockerAvailable && (
								<Button asChild>
									<Link to={ROUTES.VOLUMES}>Browse Volumes</Link>
								</Button>
							)}
							<Button asChild variant={dockerAvailable ? 'outline' : undefined}>
								<Link to={ROUTES.JOBS}>Manage Jobs</Link>
							</Button>
							<Button asChild variant="outline">
								<Link to={ROUTES.POLICIES}>Manage Policies</Link>
							</Button>
							<Button asChild variant="outline">
								<Link to={ROUTES.SNAPSHOTS}>View All Snapshots</Link>
							</Button>
						</div>
					</CardContent>
				</Card>
			</div>
		</Container>
	);
}

function StatCard({
label,
value,
loading,
icon,
iconBg,
link,
description,
}: {
label: string;
	value: string | number;
	loading: boolean;
	icon: React.ReactNode;
	iconBg: string;
	link?: { to: string; label: string };
	description?: string;
}) {
	return (
<Card>
			<CardContent className="p-4">
				<div className="flex items-center justify-between">
					<div>
						<p className="text-sm text-muted-foreground">{label}</p>
						{loading ? (
<Skeleton className="h-8 w-16 mt-1" />
						) : (
<p className="text-2xl font-semibold mt-1">{value}</p>
						)}
					</div>
					<div className={`h-10 w-10 rounded-lg ${iconBg} flex items-center justify-center`}>
						{icon}
					</div>
				</div>
				{link && (
					<p className="text-xs text-muted-foreground mt-2">
						<Link to={link.to} className="hover:underline">
							{link.label}
						</Link>
					</p>
				)}
				{description && (
					<p className="text-xs text-muted-foreground mt-2">{description}</p>
				)}
			</CardContent>
		</Card>
	);
}
