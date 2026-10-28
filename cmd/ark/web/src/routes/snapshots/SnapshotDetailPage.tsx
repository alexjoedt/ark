import { useParams, Link } from 'react-router';
import { api } from '@/lib/api';
import { useAsyncData } from '@/hooks/use-async-data';
import { ROUTES } from '@/lib/constants';
import { formatBytes } from '@/lib/utils';
import { format, isValid } from 'date-fns';
import { Container } from '@/components/layout/container';
import { PageHeader } from '@/components/layout/page-header';
import { Card, CardHeader, CardTitle, CardContent } from '@/components/ui/card';
import { Button } from '@/components/ui/button';
import { Skeleton } from '@/components/ui/skeleton';
import {
	ChevronLeft,
	Download,
	HardDrive,
	Calendar,
	FileText,
	FolderOpen,
	Database
} from 'lucide-react';

function safeFormat(dateStr: string | undefined | null, formatStr: string, fallback = '—'): string {
	if (!dateStr) return fallback;
	const d = new Date(dateStr);
	if (!isValid(d) || d.getFullYear() < 100) return fallback;
	return format(d, formatStr);
}

function DetailRow({ label, children }: { label: string; children: React.ReactNode }) {
	return (
		<div className="flex items-start justify-between py-3 border-b last:border-b-0">
			<span className="text-sm font-medium text-muted-foreground w-40 shrink-0">{label}</span>
			<div className="text-sm flex-1 text-right">{children}</div>
		</div>
	);
}

export default function SnapshotDetailPage() {
	const params = useParams();
	const id = params.id || '';

	const {
		data: snapshot,
		loading,
		error
	} = useAsyncData(() => api.getSnapshot(id), null, id);

	if (loading) {
		return (
			<Container>
				<div className="flex items-center gap-2 mb-6">
					<Button variant="ghost" size="sm" asChild>
						<Link to={ROUTES.SNAPSHOTS}>
							<ChevronLeft className="h-4 w-4 mr-1" />
							Snapshots
						</Link>
					</Button>
				</div>
				<div className="space-y-4">
					<Skeleton className="h-8 w-64" />
					<Skeleton className="h-4 w-48" />
					<Card>
						<CardContent className="pt-6 space-y-3">
							{[...Array(6)].map((_, i) => (
								<div key={i} className="flex justify-between">
									<Skeleton className="h-4 w-32" />
									<Skeleton className="h-4 w-48" />
								</div>
							))}
						</CardContent>
					</Card>
				</div>
			</Container>
		);
	}

	if (error || !snapshot) {
		return (
			<Container>
				<div className="flex items-center gap-2 mb-6">
					<Button variant="ghost" size="sm" asChild>
						<Link to={ROUTES.SNAPSHOTS}>
							<ChevronLeft className="h-4 w-4 mr-1" />
							Snapshots
						</Link>
					</Button>
				</div>
				<div className="text-center py-12">
					<Database className="h-12 w-12 mx-auto text-muted-foreground mb-4" />
					<h2 className="text-lg font-semibold mb-2">Snapshot not found</h2>
					<p className="text-sm text-muted-foreground">
						The snapshot with ID <code className="font-mono text-xs">{id}</code> could not be found.
					</p>
				</div>
			</Container>
		);
	}

	const dedupRatio =
		snapshot.size_bytes > 0 && snapshot.stored_bytes > 0
			? (snapshot.size_bytes / snapshot.stored_bytes).toFixed(2)
			: null;

	const handleExport = () => {
		const url = api.exportSnapshotUrl(snapshot.id);
		window.open(url, '_blank');
	};

	return (
		<Container>
			{/* Back navigation + actions */}
			<div className="flex items-center justify-between gap-2 mb-6">
				<Button variant="ghost" size="sm" asChild>
					<Link to={ROUTES.SNAPSHOTS}>
						<ChevronLeft className="h-4 w-4 mr-1" />
						Snapshots
					</Link>
				</Button>
				<Button variant="outline" size="sm" onClick={handleExport}>
					<Download className="h-4 w-4 mr-2" />
					Export
				</Button>
			</div>

			<PageHeader
				title={snapshot.volume}
				description={`Snapshot from ${safeFormat(snapshot.created_at, 'PPP', snapshot.id)}`}
			/>

			<div className="mt-6 grid gap-6 md:grid-cols-2">
				{/* Identity */}
				<Card>
					<CardHeader>
						<CardTitle className="text-base flex items-center gap-2">
							<FileText className="h-4 w-4" />
							Identity
						</CardTitle>
					</CardHeader>
					<CardContent className="p-0 px-6 pb-2">
						<DetailRow label="ID">
							<code className="font-mono text-xs break-all">{snapshot.id}</code>
						</DetailRow>
						<DetailRow label="Volume">
							<Link
								to={ROUTES.VOLUME_DETAIL(encodeURIComponent(snapshot.volume))}
								className="hover:underline inline-flex items-center gap-1"
							>
								<FolderOpen className="h-3.5 w-3.5" />
								{snapshot.volume}
							</Link>
						</DetailRow>
						<DetailRow label="Type">{snapshot.source_type}</DetailRow>

						{snapshot.comment && (
							<DetailRow label="Comment">{snapshot.comment}</DetailRow>
						)}
					</CardContent>
				</Card>

				{/* Size & Files */}
				<Card>
					<CardHeader>
						<CardTitle className="text-base flex items-center gap-2">
							<HardDrive className="h-4 w-4" />
							Size &amp; Contents
						</CardTitle>
					</CardHeader>
					<CardContent className="p-0 px-6 pb-2">
						<DetailRow label="Original Size">
							{formatBytes(snapshot.size_bytes)}
						</DetailRow>
						<DetailRow label="Stored Size">
							{formatBytes(snapshot.stored_bytes)}
						</DetailRow>
						{dedupRatio && (
							<DetailRow label="Dedup Ratio">{dedupRatio}×</DetailRow>
						)}
						<DetailRow label="Files">{snapshot.file_count}</DetailRow>
					</CardContent>
				</Card>

				{/* Timing */}
				<Card>
					<CardHeader>
						<CardTitle className="text-base flex items-center gap-2">
							<Calendar className="h-4 w-4" />
							Timing
						</CardTitle>
					</CardHeader>
					<CardContent className="p-0 px-6 pb-2">
						<DetailRow label="Created">
							{safeFormat(snapshot.created_at, 'PPpp')}
						</DetailRow>
					</CardContent>
				</Card>
			</div>
		</Container>
	);
}
