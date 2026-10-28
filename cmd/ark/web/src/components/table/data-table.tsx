import * as React from 'react';
import { useState, useMemo, useCallback } from 'react';
import {
	Table,
	TableHeader,
	TableHead,
	TableBody,
	TableRow,
	TableCell
} from '@/components/ui/table';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Checkbox } from '@/components/ui/checkbox';
import { Badge } from '@/components/ui/badge';
import {
	DropdownMenu,
	DropdownMenuTrigger,
	DropdownMenuContent,
	DropdownMenuItem
} from '@/components/ui/dropdown-menu';
import { cn } from '@/lib/utils';
import {
	Search,
	X,
	ChevronUp,
	ChevronDown,
	ChevronsUpDown,
	MoreHorizontal,
	ChevronLeft,
	ChevronRight,
	ChevronsLeft,
	ChevronsRight
} from 'lucide-react';

// ============================================================================
// Type Definitions
// ============================================================================

export interface ColumnDef<TData> {
	id: string;
	header: string | ((props: HeaderProps) => React.ReactNode);
	accessor?: keyof TData | ((row: TData) => any);
	cell?: (props: CellProps<TData>) => React.ReactNode;
	sortable?: boolean;
	searchable?: boolean;
	width?: string;
}

export interface CellProps<TData> {
	row: TData;
	value: any;
	rowIndex: number;
}

export interface HeaderProps {
	column: ColumnDef<any>;
	sortState?: { columnId: string; direction: 'asc' | 'desc' };
	onSort?: (direction: 'asc' | 'desc' | null) => void;
}

export interface PaginationConfig {
	currentPage: number;
	pageSize: number;
	totalItems: number;
	totalPages: number;
}

export interface SelectionConfig {
	mode: 'single' | 'multi';
	selectedIds: Set<string | number>;
	onSelectionChange: (selectedIds: Set<string | number>) => void;
}

export interface ActionsConfig<TData> {
	items: Array<{
		label: string;
		icon?: React.ReactNode;
		onClick: (row: TData) => void;
		variant?: 'default' | 'destructive';
		show?: (row: TData) => boolean;
	}>;
	position?: 'left' | 'right';
}

export interface BulkActionsConfig<TData> {
	items: Array<{
		label: string;
		icon?: React.ReactNode;
		onClick: (selectedRows: TData[], selectedIds: Set<string | number>) => void;
		variant?: 'default' | 'destructive';
		show?: (selectedCount: number) => boolean;
	}>;
}

export interface EmptyStateConfig {
	icon?: React.ReactNode;
	title: string;
	description?: string;
	action?: React.ReactNode;
}

export interface DataTableProps<TData> {
	// Required
	data: TData[];
	columns: ColumnDef<TData>[];

	// Row identity
	rowIdAccessor: (row: TData) => string | number;

	// Sorting
	sortState?: { columnId: string; direction: 'asc' | 'desc' };
	onSort?: (columnId: string, direction: 'asc' | 'desc' | null) => void;

	// Search
	searchValue?: string;
	onSearchChange?: (value: string) => void;
	searchPlaceholder?: string;
	searchableColumns?: string[];

	// Pagination
	pagination?: PaginationConfig;
	onPaginationChange?: (page: number, pageSize: number) => void;

	// Selection
	selection?: SelectionConfig;

	// Row actions
	actions?: ActionsConfig<TData>;
	bulkActions?: BulkActionsConfig<TData>;
	onRowClick?: (row: TData, event: React.MouseEvent) => void;

	// States
	loading?: boolean;
	emptyState?: EmptyStateConfig;

	// Customization
	className?: string;
	tableClassName?: string;
}

// ============================================================================
// Sub-Components
// ============================================================================

function DataTableToolbar<TData>({
	searchValue,
	onSearchChange,
	searchPlaceholder,
	selectedCount,
	bulkActions,
	selectedRows,
	selectedIds
}: {
	searchValue?: string;
	onSearchChange?: (value: string) => void;
	searchPlaceholder?: string;
	selectedCount?: number;
	bulkActions?: BulkActionsConfig<TData>;
	selectedRows?: TData[];
	selectedIds?: Set<string | number>;
}) {
	const [inputValue, setInputValue] = useState(searchValue || '');

	// Debounce search input
	React.useEffect(() => {
		const timer = setTimeout(() => {
			onSearchChange?.(inputValue);
		}, 300);

		return () => clearTimeout(timer);
	}, [inputValue, onSearchChange]);

	// Sync with external changes
	const visibleBulkActions = bulkActions?.items.filter(
		(action) => !action.show || action.show(selectedCount || 0)
	) || [];

	const hasBulkActions = selectedCount && selectedCount > 0 && visibleBulkActions.length > 0;

	return (
		<div className="flex items-center justify-between gap-2 mb-4">
			<div className="relative flex-1 max-w-sm">
				<Search className="absolute left-2 top-1/2 -translate-y-1/2 h-4 w-4 text-muted-foreground" />
				<Input
					type="text"
					placeholder={searchPlaceholder || 'Search...'}
					value={inputValue}
					onChange={(e) => setInputValue(e.target.value)}
					className="pl-8 pr-8 h-9"
				/>
				{inputValue && (
					<button
						onClick={() => setInputValue('')}
						className="absolute right-2 top-1/2 -translate-y-1/2 text-muted-foreground hover:text-foreground"
					>
						<X className="h-4 w-4" />
					</button>
				)}
			</div>
			
			{/* Bulk actions or selected count */}
			{selectedCount !== undefined && selectedCount > 0 && (
				<div className="flex items-center gap-2">
					<Badge variant="secondary" className="h-9 px-3">
						{selectedCount} selected
					</Badge>
					
					{hasBulkActions && (
						<div className="flex items-center gap-1">
							{visibleBulkActions.map((action, idx) => (
								<Button
									key={idx}
									size="sm"
									variant={action.variant === 'destructive' ? 'destructive' : 'outline'}
									onClick={() => action.onClick(selectedRows || [], selectedIds || new Set())}
									className="h-9"
								>
									{action.icon}
									{action.label}
								</Button>
							))}
						</div>
					)}
				</div>
			)}
		</div>
	);
}

function DataTableHeaderCell({
	column,
	sortState,
	onSort
}: {
	column: ColumnDef<any>;
	sortState?: { columnId: string; direction: 'asc' | 'desc' };
	onSort?: (columnId: string, direction: 'asc' | 'desc' | null) => void;
}) {
	const isSorted = sortState?.columnId === column.id;
	const direction = isSorted ? sortState.direction : null;

	const handleSort = useCallback(() => {
		if (!column.sortable || !onSort) return;

		// Cycle: null -> asc -> desc -> null
		if (!direction) {
			onSort(column.id, 'asc');
		} else if (direction === 'asc') {
			onSort(column.id, 'desc');
		} else {
			onSort(column.id, null);
		}
	}, [column.id, column.sortable, direction, onSort]);

	const headerContent =
		typeof column.header === 'function'
			? column.header({ column, sortState, onSort: handleSort })
			: column.header;

	if (!column.sortable) {
		return <TableHead className={column.width}>{headerContent}</TableHead>;
	}

	return (
		<TableHead className={column.width}>
			<button
				onClick={handleSort}
				className="flex items-center gap-1.5 hover:text-foreground transition-colors group w-full text-left"
				aria-label={`Sort by ${column.header}`}
			>
				<span>{headerContent}</span>
				<span className="text-muted-foreground group-hover:text-foreground transition-colors">
					{!direction && <ChevronsUpDown className="h-3.5 w-3.5" />}
					{direction === 'asc' && <ChevronUp className="h-3.5 w-3.5" />}
					{direction === 'desc' && <ChevronDown className="h-3.5 w-3.5" />}
				</span>
			</button>
		</TableHead>
	);
}

function DataTablePagination({
	pagination,
	onPaginationChange,
	loading
}: {
	pagination: PaginationConfig;
	onPaginationChange?: (page: number, pageSize: number) => void;
	loading?: boolean;
}) {
	const { currentPage, pageSize, totalItems, totalPages } = pagination;

	const startItem = totalItems === 0 ? 0 : (currentPage - 1) * pageSize + 1;
	const endItem = Math.min(currentPage * pageSize, totalItems);

	const pageSizeOptions = [10, 25, 50, 100];

	const handlePageChange = useCallback(
		(newPage: number) => {
			if (newPage >= 1 && newPage <= totalPages && !loading) {
				onPaginationChange?.(newPage, pageSize);
			}
		},
		[totalPages, pageSize, loading, onPaginationChange]
	);

	const handlePageSizeChange = useCallback(
		(newSize: number) => {
			if (!loading) {
				onPaginationChange?.(1, newSize);
			}
		},
		[loading, onPaginationChange]
	);

	return (
		<div className="flex items-center justify-between gap-4 pt-4 border-t">
			<div className="flex items-center gap-2 text-xs text-muted-foreground">
				<span>
					Showing {startItem}-{endItem} of {totalItems} results
				</span>
			</div>

			<div className="flex items-center gap-2">
				<div className="flex items-center gap-1.5">
					<span className="text-xs text-muted-foreground">Rows per page:</span>
					<select
						value={pageSize}
						onChange={(e) => handlePageSizeChange(Number(e.target.value))}
						disabled={loading}
						className="h-8 px-2 text-sm border border-input rounded-md bg-background hover:bg-muted/50 focus:outline-none focus:ring-2 focus:ring-ring disabled:opacity-50 disabled:cursor-not-allowed"
					>
						{pageSizeOptions.map((size) => (
							<option key={size} value={size}>
								{size}
							</option>
						))}
					</select>
				</div>

				<div className="flex items-center gap-1">
					<Button
						variant="outline"
						size="sm"
						onClick={() => handlePageChange(1)}
						disabled={currentPage === 1 || loading}
						className="h-8 w-8 p-0"
					>
						<ChevronsLeft className="h-4 w-4" />
					</Button>
					<Button
						variant="outline"
						size="sm"
						onClick={() => handlePageChange(currentPage - 1)}
						disabled={currentPage === 1 || loading}
						className="h-8 w-8 p-0"
					>
						<ChevronLeft className="h-4 w-4" />
					</Button>

					<div className="flex items-center gap-1 px-2">
						<span className="text-sm">
							Page {currentPage} of {totalPages}
						</span>
					</div>

					<Button
						variant="outline"
						size="sm"
						onClick={() => handlePageChange(currentPage + 1)}
						disabled={currentPage === totalPages || loading}
						className="h-8 w-8 p-0"
					>
						<ChevronRight className="h-4 w-4" />
					</Button>
					<Button
						variant="outline"
						size="sm"
						onClick={() => handlePageChange(totalPages)}
						disabled={currentPage === totalPages || loading}
						className="h-8 w-8 p-0"
					>
						<ChevronsRight className="h-4 w-4" />
					</Button>
				</div>
			</div>
		</div>
	);
}

function DataTableRow<TData>({
	row,
	rowId,
	rowIndex,
	columns,
	selection,
	actions,
	onRowClick,
	isSelected
}: {
	row: TData;
	rowId: string | number;
	rowIndex: number;
	columns: ColumnDef<TData>[];
	selection?: SelectionConfig;
	actions?: ActionsConfig<TData>;
	onRowClick?: (row: TData, event: React.MouseEvent) => void;
	isSelected: boolean;
}) {
	const handleRowClick = useCallback(
		(e: React.MouseEvent) => {
			// Don't trigger row click if clicking on interactive elements
			const target = e.target as HTMLElement;
			if (
				target.closest('button') ||
				target.closest('input[type="checkbox"]') ||
				target.closest('[role="button"]')
			) {
				return;
			}

			onRowClick?.(row, e);
		},
		[row, onRowClick]
	);

	const handleSelectionChange = useCallback(
		(checked: boolean) => {
			if (!selection) return;

			const newSelectedIds = new Set(selection.selectedIds);
			if (checked) {
				if (selection.mode === 'single') {
					newSelectedIds.clear();
				}
				newSelectedIds.add(rowId);
			} else {
				newSelectedIds.delete(rowId);
			}
			selection.onSelectionChange(newSelectedIds);
		},
		[selection, rowId]
	);

	const visibleActions = useMemo(
		() => actions?.items.filter((action) => !action.show || action.show(row)) || [],
		[actions, row]
	);

	return (
		<TableRow
			onClick={handleRowClick}
			className={cn(
				onRowClick && 'cursor-pointer',
				isSelected && 'bg-muted/50'
			)}
			data-state={isSelected ? 'selected' : undefined}
		>
			{/* Selection checkbox column */}
			{selection && (
				<TableCell className="w-10">
					<Checkbox
						checked={isSelected}
						onCheckedChange={handleSelectionChange}
						aria-label={`Select row ${rowIndex + 1}`}
					/>
				</TableCell>
			)}

			{/* Actions column (left) */}
			{actions?.position === 'left' && (
				<TableCell className="w-15">
					{visibleActions.length > 0 && (
						<DropdownMenu>
							<DropdownMenuTrigger asChild>
								<Button variant="ghost" size="sm" className="h-8 w-8 p-0">
									<MoreHorizontal className="h-4 w-4" />
									<span className="sr-only">Open menu</span>
								</Button>
							</DropdownMenuTrigger>
							<DropdownMenuContent align="start">
								{visibleActions.map((action, idx) => (
									<DropdownMenuItem
										key={idx}
										onClick={() => action.onClick(row)}
										className={cn(
											'flex items-center gap-2',
											action.variant === 'destructive' && 'text-destructive'
										)}
									>
										{action.icon}
										{action.label}
									</DropdownMenuItem>
								))}
							</DropdownMenuContent>
						</DropdownMenu>
					)}
				</TableCell>
			)}

			{/* Data columns */}
			{columns.map((column) => {
				const value = column.accessor
					? typeof column.accessor === 'function'
						? column.accessor(row)
						: row[column.accessor]
					: null;

				const cellContent = column.cell
					? column.cell({ row, value, rowIndex })
					: value !== null && value !== undefined
					? String(value)
					: '';

				return (
					<TableCell key={column.id} className={column.width}>
						{cellContent}
					</TableCell>
				);
			})}

			{/* Actions column (right) */}
			{(!actions?.position || actions?.position === 'right') && actions && (
				<TableCell className="w-15">
					{visibleActions.length > 0 && (
						<DropdownMenu>
							<DropdownMenuTrigger asChild>
								<Button variant="ghost" size="sm" className="h-8 w-8 p-0">
									<MoreHorizontal className="h-4 w-4" />
									<span className="sr-only">Open menu</span>
								</Button>
							</DropdownMenuTrigger>
							<DropdownMenuContent align="end">
								{visibleActions.map((action, idx) => (
									<DropdownMenuItem
										key={idx}
										onClick={() => action.onClick(row)}
										className={cn(
											'flex items-center gap-2',
											action.variant === 'destructive' && 'text-destructive'
										)}
									>
										{action.icon}
										{action.label}
									</DropdownMenuItem>
								))}
							</DropdownMenuContent>
						</DropdownMenu>
					)}
				</TableCell>
			)}
		</TableRow>
	);
}

// ============================================================================
// Main Component
// ============================================================================

/**
 * DataTable - A feature-rich, type-safe table component
 *
 * Features:
 * - Sorting (single-column, controlled)
 * - Global search/filtering
 * - Server-side pagination
 * - Row selection (single/multi)
 * - Row actions with dropdown
 * - Clickable rows
 * - Loading and empty states
 * - Sticky headers
 * - Responsive design
 *
 * @example
 * ```tsx
 * <DataTable
 *   data={volumes}
 *   columns={columns}
 *   rowIdAccessor={(row) => row.id}
 *   sortState={sortState}
 *   onSort={handleSort}
 *   searchValue={search}
 *   onSearchChange={setSearch}
 *   pagination={pagination}
 *   onPaginationChange={handlePaginationChange}
 *   onRowClick={(row) => navigate(`/items/${row.id}`)}
 * />
 * ```
 */
export function DataTable<TData>({
	data,
	columns,
	rowIdAccessor,
	sortState,
	onSort,
	searchValue,
	onSearchChange,
	searchPlaceholder,
	pagination,
	onPaginationChange,
	selection,
	actions,
	bulkActions,
	onRowClick,
	loading,
	emptyState,
	className,
	tableClassName
}: DataTableProps<TData>): React.ReactElement {

	// Calculate selection state
	const selectedCount = selection?.selectedIds.size || 0;
	const allSelected =
		selection && data.length > 0 && data.every((row) => selection.selectedIds.has(rowIdAccessor(row)));
	const someSelected = selection && selectedCount > 0 && !allSelected;

	// Get selected rows for bulk actions
	const selectedRows = useMemo(() => {
		if (!selection || selectedCount === 0) return [];
		return data.filter((row) => selection.selectedIds.has(rowIdAccessor(row)));
	}, [data, selection, selectedCount, rowIdAccessor]);

	// Handle select all
	const handleSelectAll = useCallback(
		(checked: boolean) => {
			if (!selection) return;

			const newSelectedIds = new Set(selection.selectedIds);
			if (checked) {
				// Add all rows on current page
				data.forEach((row) => {
					newSelectedIds.add(rowIdAccessor(row));
				});
			} else {
				// Remove all rows on current page
				data.forEach((row) => {
					newSelectedIds.delete(rowIdAccessor(row));
				});
			}
			selection.onSelectionChange(newSelectedIds);
		},
		[selection, data, rowIdAccessor]
	);

	// Render empty state
	if (!loading && data.length === 0) {
		if (emptyState) {
			return (
				<div className={cn('w-full', className)}>
					{onSearchChange && (
						<DataTableToolbar
							searchValue={searchValue}
							onSearchChange={onSearchChange}
							searchPlaceholder={searchPlaceholder}
						/>
					)}
					<div className="flex flex-col items-center justify-center py-12 text-center">
						<div className="text-muted-foreground mb-4">{emptyState.icon}</div>
						<h3 className="text-lg font-semibold mb-1">{emptyState.title}</h3>
						{emptyState.description && (
							<p className="text-sm text-muted-foreground mb-4 max-w-sm">{emptyState.description}</p>
						)}
						{emptyState.action && <div>{emptyState.action}</div>}
					</div>
				</div>
			);
		}

		return (
			<div className={cn('w-full', className)}>
				{onSearchChange && (
					<DataTableToolbar
						searchValue={searchValue}
						onSearchChange={onSearchChange}
						searchPlaceholder={searchPlaceholder}
					/>
				)}
				<div className="text-center py-12 text-sm text-muted-foreground">No data available</div>
			</div>
		);
	}

	return (
		<div className={cn('w-full', className)}>
			{/* Toolbar */}
			{onSearchChange && (
				<DataTableToolbar
					searchValue={searchValue}
					onSearchChange={onSearchChange}
					searchPlaceholder={searchPlaceholder}
					selectedCount={selection ? selectedCount : undefined}
					bulkActions={bulkActions}
					selectedRows={selectedRows}
					selectedIds={selection?.selectedIds}
				/>
			)}

			{/* Table */}
			<div className="relative">
				{loading && (
					<div className="absolute inset-0 bg-background/50 flex items-center justify-center z-20">
						<div className="text-sm text-muted-foreground">Loading...</div>
					</div>
				)}

				<Table className={tableClassName}>
					<TableHeader className="sticky top-0 bg-background z-10">
						<TableRow>
							{/* Selection header */}
							{selection && (
								<TableHead className="w-10">
									{selection.mode === 'multi' && (
										<Checkbox
											checked={allSelected || someSelected}
											onCheckedChange={handleSelectAll}
											aria-label="Select all rows"
										/>
									)}
								</TableHead>
							)}

							{/* Actions header (left) */}
							{actions?.position === 'left' && <TableHead className="w-15">Actions</TableHead>}

							{/* Column headers */}
							{columns.map((column) => (
								<DataTableHeaderCell
									key={column.id}
									column={column}
									sortState={sortState}
									onSort={onSort}
								/>
							))}

							{/* Actions header (right) */}
							{(!actions?.position || actions?.position === 'right') && actions && (
								<TableHead className="w-15">Actions</TableHead>
							)}
						</TableRow>
					</TableHeader>

					<TableBody>
						{data.map((row, index) => {
							const rowId = rowIdAccessor(row);
							const isSelected = selection?.selectedIds.has(rowId) || false;

							return (
								<DataTableRow
									key={rowId}
									row={row}
									rowId={rowId}
									rowIndex={index}
									columns={columns}
									selection={selection}
									actions={actions}
									onRowClick={onRowClick}
									isSelected={isSelected}
								/>
							);
						})}
					</TableBody>
				</Table>
			</div>

			{/* Pagination */}
			{pagination && onPaginationChange && (
				<DataTablePagination
					pagination={pagination}
					onPaginationChange={onPaginationChange}
					loading={loading}
				/>
			)}
		</div>
	);
}

// Export a helper for creating columns with better type inference
export function createColumnHelper<TData>() {
	return {
		accessor: <TKey extends keyof TData>(
			accessor: TKey,
			column: Omit<ColumnDef<TData>, 'accessor'>
		): ColumnDef<TData> => ({
			accessor,
			...column
		}),
		display: (column: ColumnDef<TData>): ColumnDef<TData> => column
	};
}
