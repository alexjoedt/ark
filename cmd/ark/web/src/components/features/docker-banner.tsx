import { AlertCircle, RefreshCw } from 'lucide-react';
import { Alert, AlertDescription } from '@/components/ui/alert';
import { Button } from '@/components/ui/button';
import { useDocker } from '@/lib/contexts/docker-context';
import { useState } from 'react';

export function DockerUnavailableBanner() {
	const { dockerAvailable, checkDockerStatus } = useDocker();
	const [isChecking, setIsChecking] = useState(false);

	if (dockerAvailable) {
		return null;
	}

	const handleRetry = async () => {
		setIsChecking(true);
		await checkDockerStatus();
		setIsChecking(false);
	};

	return (
		<Alert variant="destructive" className="mb-6">
			<AlertCircle className="h-4 w-4" />
			<AlertDescription className="flex items-center justify-between">
				<div>
					<strong className="font-semibold">Docker Unavailable</strong>
					<p className="mt-1">
						Cannot connect to Docker daemon. Volume operations are disabled. 
						You can still view snapshots, manage jobs, and configure policies.
					</p>
				</div>
				<Button 
					variant="outline" 
					size="sm" 
					onClick={handleRetry}
					disabled={isChecking}
					className="ml-4 flex-shrink-0"
				>
					<RefreshCw className={`h-4 w-4 mr-2 ${isChecking ? 'animate-spin' : ''}`} />
					Retry
				</Button>
			</AlertDescription>
		</Alert>
	);
}
