import { Button } from '@/components/ui/button';

interface EmptyStateProps {
	icon: React.ReactNode; // Lucide icon component
	title: string;
	description: string;
	action?: {
		label: string;
		onClick: () => void;
		disabled?: boolean;
		title?: string;
	};
}

export function EmptyState({ icon, title, description, action }: EmptyStateProps) {
	return (
		<div className="flex flex-col items-center justify-center py-12 text-center">
			<div className="text-muted-foreground mb-4">{icon}</div>
			<h3 className="text-lg font-semibold mb-1">{title}</h3>
			<p className="text-sm text-muted-foreground mb-4 max-w-sm">{description}</p>
			{action && (
				<Button
					onClick={action.onClick}
					size="sm"
					disabled={action.disabled}
					title={action.title}
				>
					{action.label}
				</Button>
			)}
		</div>
	);
}
