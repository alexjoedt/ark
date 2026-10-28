import type { ReactNode } from 'react';
import { cn } from '../../lib/utils';

interface ContainerProps {
  children: ReactNode;
  className?: string;
  size?: 'default' | 'wide' | 'full';
  noPadding?: boolean;
}

/**
 * Container component for consistent page layout and spacing.
 * Provides responsive padding and max-width constraints.
 */
export function Container({ 
  children, 
  className, 
  size = 'default',
  noPadding = false 
}: ContainerProps) {
  const sizeClasses = {
    default: 'max-w-7xl',
    wide: 'max-w-[1600px]',
    full: 'max-w-none',
  };

  return (
    <div
      className={cn(
        'mx-auto w-full',
        sizeClasses[size],
        !noPadding && 'px-6 py-6 md:px-8 md:py-8',
        className
      )}
    >
      {children}
    </div>
  );
}
