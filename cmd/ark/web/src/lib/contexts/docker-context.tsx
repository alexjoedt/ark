import { createContext, useContext, useState, useEffect, useCallback, type ReactNode } from 'react';
import { apiClient } from '../api/client';

interface DockerContextValue {
	dockerAvailable: boolean;
	loading: boolean;
	error: string | null;
	checkDockerStatus: () => Promise<void>;
}

const DockerContext = createContext<DockerContextValue | undefined>(undefined);

interface DockerProviderProps {
	children: ReactNode;
}

export function DockerProvider({ children }: DockerProviderProps) {
	const [dockerAvailable, setDockerAvailable] = useState(true);
	const [loading, setLoading] = useState(true);
	const [error, setError] = useState<string | null>(null);

	const checkDockerStatus = useCallback(async () => {
		setLoading(true);
		try {
			const response = await apiClient.get('/docker/health');
			setDockerAvailable(response.data.docker_available);
			setError(response.data.error || null);
		} catch (err: any) {
			// If the endpoint itself fails, assume Docker check failed but arkUI is running
			console.error('Failed to check Docker status:', err);
			setDockerAvailable(false);
			setError(err.message || 'Failed to check Docker status');
		} finally {
			setLoading(false);
		}
	}, []);

	// Check Docker status on mount
	useEffect(() => {
		checkDockerStatus();
	}, [checkDockerStatus]);

	return (
		<DockerContext.Provider value={{ dockerAvailable, loading, error, checkDockerStatus }}>
			{children}
		</DockerContext.Provider>
	);
}

export function useDocker() {
	const context = useContext(DockerContext);
	if (context === undefined) {
		throw new Error('useDocker must be used within a DockerProvider');
	}
	return context;
}
