import { useNavigate, useParams } from 'react-router';
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
import { Card, CardHeader, CardTitle, CardContent } from '@/components/ui/card';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';
import {
	Table,
	TableHeader,
	TableHead,
	TableBody,
	TableRow,
	TableCell
} from '@/components/ui/table';
import { EmptyState } from '@/components/features/empty-state';
import { BackupDialog } from '@/components/features/backup-dialog';
import { RestoreDialog } from '@/components/features/restore-dialog';
import { DeleteDialog } from '@/components/features/delete-dialog';
import {
	Database,
	Download,
	Trash2,
	ChevronLeft,
	Plus,
	RotateCw,
	Archive as ArchiveIcon,
	HardDrive,
	Calendar
} from 'lucide-react';
import { Skeleton } from '@/components/ui/skeleton';

export default function VolumeDetailPage() {
	const params = useParams();
	const volumeName = decodeURIComponent(params.name || '');

	const { data: allVolumes, loading: volumesLoading, refetch: refetchVolumes } = useAsyncData(
		() => api.listVolumes(),
		[],
		volumeName
	);
	const { data: allSnapshots, loading: snapshotsLoading, refetch: refetchSnapshots } = useAsyncData(
		() => api.listSnapshots(),
		[],
		volumeName
	);
	const loading = volumesLoading || snapshotsLoading;

	const refetch = () => {
		refetchVolumes();
		refetchSnapshots();
	};

	const volume = allVolumes.find((v) => v.name === volumeName);
	const snapshots = useMemo(
		() => allSnapshots
			.filter((s) => s.volume === volumeName)
			.sort((a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime()),
		[allSnapshots, volumeName]
	);

	const navigate = useNavigate();
	const { subscribe, unsubscribe } = useWebSocket();

	const [backupDialogOpen, setBackupDialogOpen] = useState(false);
	const [restoreDialogOpen, setRestoreDialogOpen] = useState(false);
	const [deleteDialogOpen, setDeleteDialogOpen] = useState(false);

	useEffect(() => {
		const handleBackupCompleted = (event: BackupCompletedEvent) => {
			if (event.data.volume === volumeName) {
				refetch();
			}
		};

		const handleRestoreCompleted = (event: any) => {
			if (event.data.volume === volumeName) {
				refetch();
			}
		};

		const handleRestoreFailed = (event: any) => {
			if (event.data.volume === volumeName) {
				refetch();
			}
		};

		subscribe('backup.completed', handleBackupCompleted as any);
		subscribe('restore.completed', handleRestoreCompleted);
		subscribe('restore.failed', handleRestoreFailed);

		return () => {
			unsubscribe('backup.completed', handleBackupCompleted as any);
			unsubscribe('restore.completed', handleRestoreCompleted);
			unsubscribe('restore.failed', handleRestoreFailed);
		};
	}, [subscribe, unsubscribe, refetch, volumeName]);
	const [selectedSnapshot, setSelectedSnapshot] = useState<Snapshot | null>(null);
	const [isDeleting, setIsDeleting] = useState(false);

	const handleRestoreClick = (snapshot: Snapshot, e: React.MouseEvent) => {
		e.stopPropagation();
		setSelectedSnapshot(snapshot);
		setRestoreDialogOpen(true);
	};

	const handleDeleteClick = (snapshot: Snapshot, e: React.MouseEvent) => {
		e.stopPropagation();
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

	const handleDownload = (snapshot: Snapshot, e: React.MouseEvent) => {
		e.stopPropagation();
		const url = api.exportSnapshotUrl(snapshot.id);
		window.location.href = url;
	};

	const totalSize = snapshots.reduce((sum, s) => sum + s.size_bytes, 0);
	const latestBackup = snapshots.length > 0 ? snapshots[0] : null;

	// While loading, show a minimal shell
	if (loading && !volume) {
		return (
			<div className="flex flex-col h-[calc(100vh-4rem)] overflow-hidden px-6 py-6 md:px-8 md:py-8 max-w-7xl mx-auto">
				<div className="flex items-center gap-4 mb-6">
					<Button variant="ghost" size="icon" onClick={() => navigate(ROUTES.VOLUMES)} className="h-9 w-9">
						<ChevronLeft className="h-5 w-5" />
					</Button>
					<div className="flex-1">
						<Skeleton className="h-8 w-48" />
						<Skeleton className="h-4 w-32 mt-2" />
					</div>
				</div>
				<div className="grid gap-4 md:grid-cols-4 mb-6">
					{[...Array(4)].map((_, i) => (
						<Card key={i}><CardContent className="p-4"><Skeleton className="h-16 w-full" /></CardContent></Card>
					))}
				</div>
				<Card className="flex-1">
					<CardContent className="p-6">
						<div className="space-y-4">
							{[...Array(3)].map((_, i) => (
								<Skeleton key={i} className="h-5 w-full" />
							))}
						</div>
					</CardContent>
				</Card>
			</div>
		);
	}

	if (!volume) {
		return (
			<div className="flex flex-col items-center justify-center h-[calc(100vh-4rem)] px-6 py-6">
				<EmptyState
					icon={<Database className="h-12 w-12" />}
					title="Volume not found"
					description={`The volume "${volumeName}" could not be found.`}
					action={{ label: 'Back to Volumes', onClick: () => navigate(ROUTES.VOLUMES) }}
				/>
			</div>
		);
	}

	return (
		<div className="flex flex-col h-[calc(100vh-4rem)] overflow-hidden px-6 py-6 md:px-8 md:py-8 max-w-7xl mx-auto">
			{/* Fixed Header */}
			<div className="flex-shrink-0">
				<div className="flex items-center gap-4 mb-6">
					<Button 
						variant="ghost" 
						size="icon" 
						onClick={() => navigate(ROUTES.VOLUMES)}
						className="h-9 w-9"
					>
						<ChevronLeft className="h-5 w-5" />
					</Button>
					<div className="flex-1">
						<div className="flex items-center gap-3">
							<h1 className="text-2xl font-semibold">{volume.name}</h1>
							<Badge variant="secondary" className="text-xs">
								{volume.driver}
							</Badge>
						</div>
						<p className="text-sm text-muted-foreground mt-1">
						Docker volume with {snapshots.length} snapshot{snapshots.length !== 1 ? 's' : ''}
						</p>
					</div>
					<Button onClick={() => setBackupDialogOpen(true)}>
						<Plus className="h-4 w-4 mr-2" />
						Create Backup
					</Button>
				</div>

				{/* Summary Cards */}
				<div className="grid gap-4 md:grid-cols-4 mb-6">
					<Card>
						<CardContent className="p-4">
							<div className="flex items-center justify-between">
								<div>
							<p className="text-sm text-muted-foreground">Total Snapshots</p>
							<p className="text-2xl font-semibold mt-1">{snapshots.length}</p>
								</div>
								<div className="h-10 w-10 rounded-lg bg-primary/10 flex items-center justify-center">
									<ArchiveIcon className="h-5 w-5 text-primary" />
								</div>
							</div>
						</CardContent>
					</Card>
					<Card>
						<CardContent className="p-4">
							<div className="flex items-center justify-between">
								<div>
									<p className="text-sm text-muted-foreground">Total Size</p>
									<p className="text-2xl font-semibold mt-1">{formatBytes(totalSize)}</p>
								</div>
								<div className="h-10 w-10 rounded-lg bg-blue-500/10 flex items-center justify-center">
									<HardDrive className="h-5 w-5 text-blue-600 dark:text-blue-400" />
								</div>
							</div>
						</CardContent>
					</Card>
					<Card>
						<CardContent className="p-4">
							<div className="flex items-center justify-between">
								<div>
									<p className="text-sm text-muted-foreground">Latest Backup</p>
									<p className="text-lg font-semibold mt-1">
										{latestBackup 
											? formatDistanceToNow(new Date(latestBackup.created_at), { addSuffix: true })
											: 'Never'
										}
									</p>
								</div>
								<div className="h-10 w-10 rounded-lg bg-green-500/10 flex items-center justify-center">
									<Calendar className="h-5 w-5 text-green-600 dark:text-green-400" />
								</div>
							</div>
						</CardContent>
					</Card>
					<Card>
						<CardContent className="p-4">
							<div className="flex items-center justify-between">
								<div>
									<p className="text-sm text-muted-foreground">Volume Created</p>
									<p className="text-sm font-medium mt-1">
										{formatDistanceToNow(new Date(volume.created_at), { addSuffix: true })}
									</p>
								</div>
								<div className="h-10 w-10 rounded-lg bg-purple-500/10 flex items-center justify-center">
									<Database className="h-5 w-5 text-purple-600 dark:text-purple-400" />
								</div>
							</div>
							<p className="text-xs text-muted-foreground mt-2">
								{format(new Date(volume.created_at), 'MMM d, yyyy')}
							</p>
						</CardContent>
					</Card>
				</div>
			</div>

			{/* Scrollable Content */}
			<Card className="flex flex-col flex-1 min-h-0">
				<CardHeader className="flex-shrink-0 border-b">
					<div className="flex items-center justify-between">
				<CardTitle className="text-lg">Snapshots</CardTitle>
					{snapshots.length > 0 && (
						<span className="text-sm text-muted-foreground">
							{snapshots.length} snapshot{snapshots.length !== 1 ? 's' : ''}
							</span>
						)}
					</div>
				</CardHeader>
				<CardContent className="flex-1 overflow-auto p-0">
					{snapshots.length === 0 ? (
						<div className="flex items-center justify-center h-full p-8">
							<EmptyState
								icon={<Database className="h-12 w-12" />}
								title="No backups yet"
								description="Create your first backup to get started"
								action={{
									label: 'Create Backup',
									onClick: () => setBackupDialogOpen(true)
								}}
							/>
						</div>
					) : (
						<Table>
							<TableHeader className="sticky top-0 bg-background z-10">
								<TableRow>
									<TableHead className="bg-background w-[35%]">Snapshot ID</TableHead>
									<TableHead className="bg-background">Size</TableHead>
									<TableHead className="bg-background">Created</TableHead>
									<TableHead className="bg-background">Files</TableHead>
									<TableHead className="text-right bg-background">Actions</TableHead>
								</TableRow>
							</TableHeader>
							<TableBody>
								{snapshots.map((snapshot) => (
									<TableRow 
										key={snapshot.id}
										className="hover:bg-muted/50 transition-colors"
									>
										<TableCell>
											<div className="flex flex-col gap-1">
												<span className="text-sm font-medium font-mono">
													{snapshot.id}
												</span>
												{snapshot.comment && (
													<span className="text-xs text-muted-foreground">
														{snapshot.comment}
													</span>
												)}
											</div>
										</TableCell>
										<TableCell>
											<span className="text-sm">{formatBytes(snapshot.size_bytes)}</span>
										</TableCell>
										<TableCell>
											<div className="flex flex-col gap-0.5">
												<span className="text-sm">
													{formatDistanceToNow(new Date(snapshot.created_at), {
														addSuffix: true
													})}
												</span>
												<span className="text-xs text-muted-foreground">
													{format(new Date(snapshot.created_at), 'MMM d, yyyy HH:mm')}
												</span>
											</div>
										</TableCell>
										<TableCell>
											<span className="text-sm">{snapshot.file_count}</span>
										</TableCell>
										<TableCell className="text-right">
											<div className="flex items-center justify-end gap-2">
												<Button
													size="sm"
													variant="outline"
													onClick={(e) => handleDownload(snapshot, e)}
													className="h-8"
												>
													<Download className="h-3.5 w-3.5 mr-1.5" />
													Export
												</Button>
												<Button
													size="sm"
													variant="outline"
													onClick={(e) => handleRestoreClick(snapshot, e)}
													className="h-8"
												>
													<RotateCw className="h-3.5 w-3.5 mr-1.5" />
													Restore
												</Button>
												<Button
													size="sm"
													variant="ghost"
													onClick={(e) => handleDeleteClick(snapshot, e)}
													className="h-8 text-destructive hover:text-destructive hover:bg-destructive/10"
												>
													<Trash2 className="h-4 w-4" />
												</Button>
											</div>
										</TableCell>
									</TableRow>
								))}
							</TableBody>
						</Table>
					)}
				</CardContent>
			</Card>

			{/* Dialogs */}
			<BackupDialog
				volumeName={volume.name}
				open={backupDialogOpen}
				onOpenChange={setBackupDialogOpen}
			/>

			{selectedSnapshot && (
				<>
					<RestoreDialog
						snapshot={selectedSnapshot}
						volumes={allVolumes}
						open={restoreDialogOpen}
						onOpenChange={setRestoreDialogOpen}
						onRestoreComplete={() => refetch()}
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
		</div>
	);
}
