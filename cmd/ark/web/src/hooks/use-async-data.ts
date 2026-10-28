import { useState, useEffect, useCallback, useRef } from 'react';

interface UseAsyncDataResult<T> {
	data: T;
	loading: boolean;
	error: Error | null;
	refetch: () => void;
}

interface UseAsyncDataOptions {
	/**
	 * When false the fetch is skipped entirely and the hook returns the
	 * initialData with loading: false.  Useful for guarding Docker-dependent
	 * fetches behind the DockerContext availability flag.
	 *
	 * The hook re-fires when this transitions from false → true so data is
	 * loaded automatically once the gate opens.
	 *
	 * @default true
	 */
	enabled?: boolean;
}

/**
 * Hook for non-blocking async data fetching within components.
 * Pages render instantly with loading states while data is fetched in the background.
 *
 * @param fetcher - Async function that returns data
 * @param initialData - Initial data value shown before the first fetch completes
 * @param key - Optional key that triggers re-fetch when changed (e.g., route param)
 * @param options - Optional configuration (e.g. `enabled` to gate the fetch)
 */
export function useAsyncData<T>(
	fetcher: () => Promise<T>,
	initialData: T,
	key?: string,
	options?: UseAsyncDataOptions
): UseAsyncDataResult<T> {
	const enabled = options?.enabled ?? true;
	const [data, setData] = useState<T>(initialData);
	// Start with loading=true only when the fetch is enabled from the start.
	// This prevents an unnecessary loading flash when `enabled` begins as false.
	const [loading, setLoading] = useState(enabled);
	const [error, setError] = useState<Error | null>(null);
	const mountedRef = useRef(true);
	const fetcherRef = useRef(fetcher);
	fetcherRef.current = fetcher;

	const refetch = useCallback(async () => {
		if (!mountedRef.current) return;
		setLoading(true);
		setError(null);
		try {
			const result = await fetcherRef.current();
			if (mountedRef.current) {
				setData(result);
			}
		} catch (err: any) {
			if (mountedRef.current) {
				setError(err);
				console.error('[useAsyncData] Fetch error:', err);
			}
		} finally {
			if (mountedRef.current) {
				setLoading(false);
			}
		}
	}, []);

	useEffect(() => {
		mountedRef.current = true;
		if (!enabled) {
			// Clear any stale loading state and skip fetch.
			setLoading(false);
			return () => { mountedRef.current = false; };
		}
		refetch();
		return () => {
			mountedRef.current = false;
		};
		// eslint-disable-next-line react-hooks/exhaustive-deps
	}, [refetch, key, enabled]);

	return { data, loading, error, refetch };
}
