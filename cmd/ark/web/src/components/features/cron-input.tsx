import { useState, useEffect } from 'react';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Button } from '@/components/ui/button';
import { CRON_PRESETS } from '@/lib/constants';
import { cn } from '@/lib/utils';
import { CheckCircle, XCircle } from 'lucide-react';

interface CronInputProps {
  value: string;
  onChange: (value: string) => void;
  error?: string;
}

// Simple cron validation regex
const CRON_REGEX = /^(@(hourly|daily|weekly|monthly|yearly)|((\*|[0-9,\-\/]+)\s+){4}(\*|[0-9,\-\/]+))$/;

// Basic cron description generator
function describeCron(cronExpression: string): string {
  const trimmed = cronExpression.trim();
  
  // Handle special syntax
  if (trimmed === '@hourly') return 'Every hour';
  if (trimmed === '@daily') return 'Every day at midnight';
  if (trimmed === '@weekly') return 'Every Sunday at midnight';
  if (trimmed === '@monthly') return 'First day of every month at midnight';
  if (trimmed === '@yearly') return 'January 1st at midnight';
  
  // Parse standard cron format: minute hour day month weekday
  const parts = trimmed.split(/\s+/);
  if (parts.length !== 5) return 'Invalid format';
  
  const [minute, hour, day, month, weekday] = parts;
  
  // Simple descriptions for common patterns
  if (minute === '*' && hour === '*' && day === '*' && month === '*' && weekday === '*') {
    return 'Every minute';
  }
  
  if (minute !== '*' && hour === '*' && day === '*' && month === '*' && weekday === '*') {
    return `Every hour at minute ${minute}`;
  }
  
  if (minute !== '*' && hour !== '*' && day === '*' && month === '*' && weekday === '*') {
    return `Every day at ${hour.padStart(2, '0')}:${minute.padStart(2, '0')}`;
  }
  
  if (minute !== '*' && hour !== '*' && day === '*' && month === '*' && weekday !== '*') {
    const days = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];
    const dayName = days[parseInt(weekday)] || `day ${weekday}`;
    return `Every ${dayName} at ${hour.padStart(2, '0')}:${minute.padStart(2, '0')}`;
  }
  
  if (minute !== '*' && hour !== '*' && day !== '*' && month === '*' && weekday === '*') {
    return `Day ${day} of every month at ${hour.padStart(2, '0')}:${minute.padStart(2, '0')}`;
  }
  
  // Generic description
  return `At ${hour}:${minute} on day ${day} of month ${month}, weekday ${weekday}`;
}

export function CronInput({ value, onChange, error }: CronInputProps) {
  const [isValid, setIsValid] = useState<boolean>(true);
  const [description, setDescription] = useState<string>('');

  useEffect(() => {
    const valid = CRON_REGEX.test(value);
    setIsValid(valid);
    if (valid && value) {
      setDescription(describeCron(value));
    } else {
      setDescription('');
    }
  }, [value]);

  return (
    <div className="space-y-3">
      <div>
        <Label htmlFor="cron-input">Schedule</Label>
        <div className="relative mt-1.5">
          <Input
            id="cron-input"
            value={value}
            onChange={(e) => onChange(e.target.value)}
            placeholder="0 * * * *"
            className={cn(
              'font-mono pr-10',
              error && 'border-destructive'
            )}
          />
          <div className="absolute right-3 top-1/2 -translate-y-1/2">
            {value && (
              isValid ? (
                <CheckCircle className="h-4 w-4 text-green-500" />
              ) : (
                <XCircle className="h-4 w-4 text-destructive" />
              )
            )}
          </div>
        </div>
        {description && (
          <p className="text-sm text-muted-foreground mt-1">{description}</p>
        )}
        {error && (
          <p className="text-sm text-destructive mt-1">{error}</p>
        )}
      </div>

      <div>
        <Label className="text-sm text-muted-foreground">Quick Presets</Label>
        <div className="flex flex-wrap gap-2 mt-1.5">
          {CRON_PRESETS.map((preset) => (
            <Button
              key={preset.value}
              type="button"
              variant={value === preset.value ? 'default' : 'outline'}
              size="sm"
              onClick={() => onChange(preset.value)}
            >
              {preset.label}
            </Button>
          ))}
        </div>
      </div>
    </div>
  );
}
