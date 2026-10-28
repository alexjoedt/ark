import { Link } from 'react-router';
import { useState, useEffect, useMemo } from 'react';
import { api } from '@/lib/api';
import { useWebSocket } from '@/lib/contexts/websocket-context';
import { useAsyncData } from '@/hooks/use-async-data';
import type { BackupCompletedEvent } from '@/lib/types';
import { ROUTES } from '@/lib/constants';
import { formatBytes } from '@/lib/utils';
import { format, formatDistanceToNow } from 'date-fns';
import { toast } from 'sonner';
import type { Snapshot } from '@/lib/types';
import { Container } from '@/components/layout/container';
import { PageHeader } from '@/components/layout/page-header';
import { Card, CardHeader, CardTitle, CardContent } from '@/components/ui/card';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import {
	Table,
	TableHeader,
	TableHead,
	TableBody,
	TableRow,
	TableCell
} from '@/components/ui/table';
import {
	Select,
	SelectContent,
	SelectItem,
	SelectTrigger,
	SelectValue
} from '@/components/ui/select';
import { EmptyState } from '@/components/features/empty-state';
import { RestoreDialog } from '@/components/features/restore-dialog';
import { DeleteDialog } from '@/components/features/delete-dialog';
import { ImportSnapshotDialog } from '@/components/features/import-snapshot-dialog';
import { Skeleton } from '@/components/ui/skeleton';
import {
	Database,
	Search,
	Download,
	Trash2,
	RotateCw,
	FolderOpen,
	Upload
} from 'lucide-react';
import { useDocker } from '@/lib/contexts/docker-context';

export default function SnapshotsPage() {
	const { data: rawSnapshots, loading: snapshotsLoading, refetch } = useAsyncData(
		() => api.listSnapshots(),
		[]
	);
	const { dockerAvailable, loading: dockerLoading } = useDocker();
	const { data: volumes } = useAsyncData(
		() => api.listVolumes(),
		[],
		undefined,
		{ enabled: !dockerLoading && dockerAvailable }
	);
	const loading = snapshotsLoading;

	// Sort snapshots by created_at descending (newest first)
	const snapshots = useMemo(
		() => [...rawSnapshots].sort((a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime()),
		[rawSnapshots]
	);

	const { subscribe, unsubscribe } = useWebSocket();

	const [searchTerm, setSearchTerm] = useState('');
	const [volumeFilter, setVolumeFilter] = useState<string>('all');
	const [restoreDialogOpen, setRestoreDialogOpen] = useState(false);
	const [importDialogOpen, setImportDialogOpen] = useState(false);

	// Subscribe to backup.completed and restore events to refresh snapshots list
	useEffect(() => {
		const handleBackupCompleted = (_event: BackupCompletedEvent) => {
			refetch();
		};

		const handleRestoreCompleted = (_event: any) => {
			refetch();
		};

		const handleRestoreFailed = (_event: any) => {
			refetch();
		};

		subscribe('backup.completed', handleBackupCompleted as any);
		subscribe('restore.completed', handleRestoreCompleted);
		subscribe('restore.failed', handleRestoreFailed);

		return () => {
			unsubscribe('backup.completed', handleBackupCompleted as any);
			unsubscribe('restore.completed', handleRestoreCompleted);
			unsubscribe('restore.failed', handleRestoreFailed);
		};
	}, [subscribe, unsubscribe, refetch]);

	const [deleteDialogOpen, setDeleteDialogOpen] = useState(false);
	const [selectedSnapshot, setSelectedSnapshot] = useState<Snapshot | null>(null);
	const [isDeleting, setIsDeleting] = useState(false);

	// Get unique volume names for filter
	const uniqueVolumes = Array.from(new Set(snapshots.map((s) => s.volume)));

	// Filter snapshots
	const filteredSnapshots = snapshots.filter((snapshot) => {
		const matchesSearch =
			(snapshot.id ?? '').toLowerCase().includes(searchTerm.toLowerCase()) ||
			(snapshot.volume ?? '').toLowerCase().includes(searchTerm.toLowerCase());
		const matchesVolume = volumeFilter === 'all' || snapshot.volume === volumeFilter;
		return matchesSearch && matchesVolume;
	});

	const handleRestoreClick = (snapshot: Snapshot) => {
		setSelectedSnapshot(snapshot);
		setRestoreDialogOpen(true);
	};

	const handleDeleteClick = (snapshot: Snapshot) => {
		setSelectedSnapshot(snapshot);
		setDeleteDialogOpen(true);
	};

	const handleDeleteConfirm = async () => {
		if (!selectedSnapshot) return;

		setIsDeleting(true);
		try {
			await api.deleteSnapshot(selectedSnapshot.id);
			toast.success('Snapshot deleted', {
				description: `Snapshot ${selectedSnapshot.id} has been removed`
			});
			setDeleteDialogOpen(false);
			setSelectedSnapshot(null);
			refetch();
		} catch (err: any) {
			const errorMessage = err.response?.data?.error || err.message || 'Failed to delete snapshot';
			toast.error('Delete failed', { description: errorMessage });
		} finally {
			setIsDeleting(false);
		}
	};

	const handleDownload = (snapshot: Snapshot) => {
		const url = api.exportSnapshotUrl(snapshot.id);
		window.location.href = url;
	};

	const totalSize = filteredSnapshots.reduce((sum, s) => sum + s.size_bytes, 0);

	return (
		<Container className="flex flex-col h-[calc(100vh-4rem)] overflow-hidden">
			{/* Fixed Page Header - Never scrolls */}
			<div className="flex-shrink-0">
				<PageHeader
					title="All Snapshots"
					description="Browse and manage all backup snapshots across volumes"
					className="flex-shrink-0"
				/>

				{/* Summary Cards */}
				<div className="grid gap-4 md:grid-cols-3 mb-6">
					<Card>
						<CardHeader className="pb-3">
							<CardTitle className="text-sm font-medium">Total Snapshots</CardTitle>
						</CardHeader>
						<CardContent>
							{loading ? (
								<Skeleton className="h-8 w-12" />
							) : (
								<>
									<div className="text-2xl font-bold">{filteredSnapshots.length}</div>
									{volumeFilter !== 'all' && (
										<p className="text-xs text-muted-foreground mt-1">
											of {snapshots.length} total
										</p>
									)}
								</>
							)}
						</CardContent>
					</Card>
					<Card>
						<CardHeader className="pb-3">
							<CardTitle className="text-sm font-medium">Total Size</CardTitle>
						</CardHeader>
						<CardContent>
							{loading ? (
								<Skeleton className="h-8 w-16" />
							) : (
								<div className="text-2xl font-bold">{formatBytes(totalSize)}</div>
							)}
						</CardContent>
					</Card>
					<Card>
						<CardHeader className="pb-3">
							<CardTitle className="text-sm font-medium">Volumes</CardTitle>
						</CardHeader>
						<CardContent>
							{loading ? (
								<Skeleton className="h-8 w-8" />
							) : (
								<>
									<div className="text-2xl font-bold">{uniqueVolumes.length}</div>
									<p className="text-xs text-muted-foreground mt-1">with backups</p>
								</>
							)}
						</CardContent>
					</Card>
				</div>
			</div>

			{/* Main Content Card - Grows to fill space */}
			<Card className="flex flex-col flex-1 min-h-0">
				{/* Sticky Card Header - Fixed at top of card */}
				<CardHeader className="flex-shrink-0 border-b">
					<div className="flex items-center justify-between">
						<CardTitle>Snapshots ({filteredSnapshots.length})</CardTitle>
						<div className="flex items-center gap-4">
							<Button onClick={() => setImportDialogOpen(true)}>
								<Upload className="h-4 w-4 mr-2" />
								Import Snapshot
							</Button>
							<div className="relative w-64">
								<Search className="absolute left-2.5 top-2.5 h-4 w-4 text-muted-foreground" />
								<Input
									type="search"
									placeholder="Search snapshots..."
									value={searchTerm}
									onChange={(e) => setSearchTerm(e.target.value)}
									className="pl-8"
								/>
							</div>
							<Select value={volumeFilter} onValueChange={setVolumeFilter}>
								<SelectTrigger className="w-[200px]">
									<SelectValue placeholder="Filter by volume" />
								</SelectTrigger>
								<SelectContent>
									<SelectItem value="all">All Volumes</SelectItem>
									{uniqueVolumes.map((vol) => (
										<SelectItem key={vol} value={vol}>
											{vol}
										</SelectItem>
									))}
								</SelectContent>
							</Select>
						</div>
					</div>
				</CardHeader>

				{/* Scrollable Content Area */}
				<CardContent className="flex-1 overflow-auto p-0">
					{loading ? (
						<div className="p-6 space-y-4">
							{[...Array(5)].map((_, i) => (
								<div key={i} className="flex items-center gap-4">
									<Skeleton className="h-5 w-[30%]" />
									<Skeleton className="h-5 w-[15%]" />
									<Skeleton className="h-5 w-[10%]" />
									<Skeleton className="h-5 w-[20%]" />
									<Skeleton className="h-5 w-[15%]" />
								</div>
							))}
						</div>
					) : filteredSnapshots.length === 0 ? (
						<div className="flex items-center justify-center h-full p-8">
							<EmptyState
								icon={<Database className="h-12 w-12" />}
								title={searchTerm || volumeFilter !== 'all' ? 'No snapshots found' : 'No snapshots'}
								description={
									searchTerm || volumeFilter !== 'all'
										? 'Try adjusting your filters'
										: 'Create a backup to get started'
								}
							/>
						</div>
					) : (
						<div className="relative">
							<Table>
								<TableHeader className="sticky top-0 bg-background z-10">
									<TableRow>
										<TableHead className="bg-background">Date</TableHead>
										<TableHead className="bg-background">Volume</TableHead>
										<TableHead className="bg-background">Type</TableHead>
										<TableHead className="bg-background">Size</TableHead>
										<TableHead className="bg-background">Files</TableHead>
										<TableHead className="bg-background text-right">Actions</TableHead>
									</TableRow>
								</TableHeader>
								<TableBody>
									{filteredSnapshots.map((snapshot) => (
										<TableRow key={snapshot.id}>
											<TableCell className="font-medium">
												<Link
													to={ROUTES.SNAPSHOT_DETAIL(snapshot.id)}
													className="hover:underline"
												>
													<div className="flex flex-col gap-0.5">
														<span className="text-sm">
															{format(new Date(snapshot.created_at), 'PPp')}
														</span>
														<span className="text-xs text-muted-foreground">
															{formatDistanceToNow(new Date(snapshot.created_at), {
																addSuffix: true
															})}
														</span>
													</div>
												</Link>
											</TableCell>
											<TableCell>
												<div className="flex flex-col gap-0.5">
													<Link
														to={`${ROUTES.VOLUMES}/${encodeURIComponent(snapshot.volume)}`}
														className="hover:underline inline-flex items-center gap-1"
													>
														<FolderOpen className="h-3.5 w-3.5" />
														<span>{snapshot.volume}</span>
													</Link>
													{snapshot.comment && (
														<span className="text-xs text-muted-foreground">{snapshot.comment}</span>
													)}
												</div>
											</TableCell>
											<TableCell>
												<Badge variant="secondary">{snapshot.source_type}</Badge>
											</TableCell>
											<TableCell>{formatBytes(snapshot.stored_bytes || snapshot.size_bytes)}</TableCell>
											<TableCell>
												<span className="text-sm">{snapshot.file_count}</span>
											</TableCell>
											<TableCell className="text-right">
												<div className="flex items-center justify-end gap-2">
													<Button
														size="sm"
														variant="outline"
														onClick={() => handleDownload(snapshot)}
													>
														<Download className="h-4 w-4" />
													</Button>
													<Button
														size="sm"
														variant="outline"
														onClick={() => handleRestoreClick(snapshot)}
													>
														<RotateCw className="h-4 w-4" />
													</Button>
													<Button
														size="sm"
														variant="destructive"
														onClick={() => handleDeleteClick(snapshot)}
													>
														<Trash2 className="h-4 w-4" />
													</Button>
												</div>
											</TableCell>
										</TableRow>
									))}
								</TableBody>
							</Table>
						</div>
					)}
				</CardContent>
			</Card>

			{/* Dialogs */}
			{selectedSnapshot && (
				<>
					<RestoreDialog
						snapshot={selectedSnapshot}
						volumes={volumes}
						open={restoreDialogOpen}
						onOpenChange={setRestoreDialogOpen}
					/>
					<DeleteDialog
						title="Delete Snapshot?"
						description={`Are you sure you want to delete snapshot ${selectedSnapshot.id}? This action cannot be undone.`}
						onConfirm={handleDeleteConfirm}
						open={deleteDialogOpen}
						onOpenChange={setDeleteDialogOpen}
						isLoading={isDeleting}
					/>
				</>
			)}

			<ImportSnapshotDialog
				open={importDialogOpen}
				onOpenChange={setImportDialogOpen}
				onImportSuccess={() => refetch()}
			/>
		</Container>
	);
}
