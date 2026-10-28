import { Badge } from '@/components/ui/badge';
import {
	CheckCircle,
	XCircle,
	Clock,
	AlertCircle,
	Loader2,
} from 'lucide-react';
import type { ComponentProps } from 'react';

export type StatusType = 'success' | 'failed' | 'pending' | 'warning' | 'running' | 'idle';

interface StatusBadgeProps extends Omit<ComponentProps<typeof Badge>, 'variant'> {
	status: StatusType;
	showIcon?: boolean;
}

const statusConfig: Record<
	StatusType,
	{
		label: string;
		variant: ComponentProps<typeof Badge>['variant'];
		icon: typeof CheckCircle;
	}
> = {
	success: {
		label: 'Success',
		variant: 'default',
		icon: CheckCircle,
	},
	failed: {
		label: 'Failed',
		variant: 'destructive',
		icon: XCircle,
	},
	pending: {
		label: 'Pending',
		variant: 'secondary',
		icon: Clock,
	},
	warning: {
		label: 'Warning',
		variant: 'outline',
		icon: AlertCircle,
	},
	running: {
		label: 'Running',
		variant: 'default',
		icon: Loader2,
	},
	idle: {
		label: 'Idle',
		variant: 'secondary',
		icon: Clock,
	},
};

export function StatusBadge({
	status,
	showIcon = true,
	className,
	...props
}: StatusBadgeProps) {
	const config = statusConfig[status];
	const IconComponent = config.icon;

	return (
		<Badge variant={config.variant} className={className} {...props}>
			{showIcon && (
				<IconComponent
					className={`mr-1 h-3 w-3 ${status === 'running' ? 'animate-spin' : ''}`}
				/>
			)}
			{config.label}
		</Badge>
	);
}
