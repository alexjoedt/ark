import { useState } from 'react';
import { toast } from 'sonner';
import { api } from '@/lib/api';
import { useDocker } from '@/lib/contexts/docker-context';
import {
	Dialog,
	DialogContent,
	DialogDescription,
	DialogFooter,
	DialogHeader,
	DialogTitle
} from '@/components/ui/dialog';
import { Button } from '@/components/ui/button';
import { Alert, AlertDescription } from '@/components/ui/alert';
import { AlertCircle } from 'lucide-react';

interface BackupDialogProps {
	volumeName: string;
	open: boolean;
	onOpenChange: (open: boolean) => void;
}

export function BackupDialog({ volumeName, open, onOpenChange }: BackupDialogProps) {
	const [isSubmitting, setIsSubmitting] = useState(false);
	const [error, setError] = useState<string | null>(null);
	const { dockerAvailable } = useDocker();

	const handleSubmit = async () => {
		if (!dockerAvailable) {
			setError('Docker is not available. Cannot perform backup operation.');
			return;
		}

		setError(null);
		setIsSubmitting(true);

		try {
			await api.backup(volumeName);
			toast.success(`Backup started for ${volumeName}`, {
				description: 'You will be notified when the backup completes'
			});
			onOpenChange(false);
		} catch (err: any) {
			const errorMessage = err.response?.data?.error || err.message || 'Failed to start backup';
			setError(errorMessage);
			toast.error('Backup failed', { description: errorMessage });
		} finally {
			setIsSubmitting(false);
		}
	};

	return (
		<Dialog open={open} onOpenChange={onOpenChange}>
			<DialogContent>
				<DialogHeader>
					<DialogTitle>Backup Volume</DialogTitle>
					<DialogDescription>
					Create a snapshot of the Docker volume <strong>{volumeName}</strong>.
						This will stop any containers using the volume during the backup process.
					</DialogDescription>
				</DialogHeader>

				{error && (
					<Alert variant="destructive">
						<AlertCircle className="h-4 w-4" />
						<AlertDescription>{error}</AlertDescription>
					</Alert>
				)}

				{!dockerAvailable && !error && (
					<Alert variant="destructive">
						<AlertCircle className="h-4 w-4" />
						<AlertDescription>
							Docker is not available. Backup operations are disabled until Docker is accessible.
						</AlertDescription>
					</Alert>
				)}

				<DialogFooter>
					<Button variant="outline" onClick={() => onOpenChange(false)} disabled={isSubmitting}>
						Cancel
					</Button>
					<Button onClick={handleSubmit} disabled={isSubmitting || !dockerAvailable}>
						{isSubmitting ? 'Starting...' : 'Start Backup'}
					</Button>
				</DialogFooter>
			</DialogContent>
		</Dialog>
	);
}
