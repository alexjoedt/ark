import { useSearchParams } from 'react-router';
import { useCallback, useMemo } from 'react';

export interface TableState {
	search?: string;
	sort?: { columnId: string; direction: 'asc' | 'desc' };
	page?: number;
	pageSize?: number;
}

export interface UseTableStateOptions {
	defaultPageSize?: number;
	defaultSort?: { columnId: string; direction: 'asc' | 'desc' };
}

/**
 * Hook to persist table state (search, sort, pagination) in URL search params
 * 
 * @example
 * ```tsx
 * const { search, sort, pagination, setSearch, setSort, setPagination } = useTableState({
 *   defaultPageSize: 25,
 *   defaultSort: { columnId: 'name', direction: 'asc' }
 * });
 * 
 * <DataTable
 *   searchValue={search}
 *   onSearchChange={setSearch}
 *   sortState={sort}
 *   onSort={setSort}
 *   pagination={pagination}
 *   onPaginationChange={setPagination}
 * />
 * ```
 */
export function useTableState(options: UseTableStateOptions = {}) {
	const { defaultPageSize = 25, defaultSort } = options;
	const [searchParams, setSearchParams] = useSearchParams();

	// Parse state from URL
	const state = useMemo((): TableState => {
		const search = searchParams.get('search') || undefined;
		const sortColumn = searchParams.get('sort');
		const sortDirection = searchParams.get('order') as 'asc' | 'desc' | null;
		const page = parseInt(searchParams.get('page') || '1', 10);
		const pageSize = parseInt(searchParams.get('pageSize') || String(defaultPageSize), 10);

		return {
			search,
			sort: sortColumn && sortDirection ? { columnId: sortColumn, direction: sortDirection } : defaultSort,
			page,
			pageSize
		};
	}, [searchParams, defaultPageSize, defaultSort]);

	// Update search in URL
	const setSearch = useCallback(
		(search: string) => {
			setSearchParams((prev) => {
				const newParams = new URLSearchParams(prev);
				if (search) {
					newParams.set('search', search);
					// Reset to page 1 when searching
					newParams.set('page', '1');
				} else {
					newParams.delete('search');
				}
				return newParams;
			});
		},
		[setSearchParams]
	);

	// Update sort in URL
	const setSort = useCallback(
		(columnId: string, direction: 'asc' | 'desc' | null) => {
			setSearchParams((prev) => {
				const newParams = new URLSearchParams(prev);
				if (direction) {
					newParams.set('sort', columnId);
					newParams.set('order', direction);
				} else {
					newParams.delete('sort');
					newParams.delete('order');
				}
				return newParams;
			});
		},
		[setSearchParams]
	);

	// Update pagination in URL
	const setPagination = useCallback(
		(page: number, pageSize: number) => {
			setSearchParams((prev) => {
				const newParams = new URLSearchParams(prev);
				newParams.set('page', String(page));
				if (pageSize !== defaultPageSize) {
					newParams.set('pageSize', String(pageSize));
				} else {
					newParams.delete('pageSize');
				}
				return newParams;
			});
		},
		[setSearchParams, defaultPageSize]
	);

	// Clear all table state
	const clearState = useCallback(() => {
		setSearchParams((prev) => {
			const newParams = new URLSearchParams(prev);
			newParams.delete('search');
			newParams.delete('sort');
			newParams.delete('order');
			newParams.delete('page');
			newParams.delete('pageSize');
			return newParams;
		});
	}, [setSearchParams]);

	return {
		// Current state
		search: state.search,
		sort: state.sort,
		page: state.page || 1,
		pageSize: state.pageSize || defaultPageSize,

		// Setters
		setSearch,
		setSort,
		setPagination,
		clearState
	};
}
