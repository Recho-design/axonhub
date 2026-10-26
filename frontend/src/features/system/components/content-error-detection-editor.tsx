'use client';

import React, { useCallback, useMemo } from 'react';
import { IconPlus, IconTrash } from '@tabler/icons-react';
import { AlertCircle } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { cn } from '@/lib/utils';
import { type RetryableErrorPattern } from '../data/system';
import { goRegexValidationError } from '@/lib/go-regexp';

interface ContentErrorDetectionEditorProps {
  patterns: RetryableErrorPattern[];
  onChange: (patterns: RetryableErrorPattern[]) => void;
  disabled?: boolean;
}

export function ContentErrorDetectionEditor({ patterns, onChange, disabled = false }: ContentErrorDetectionEditorProps) {
  const { t } = useTranslation();

  const handleAddPattern = useCallback(() => {
    onChange([...patterns, { pattern: '', regex: true }]);
  }, [onChange, patterns]);

  const handleRemovePattern = useCallback(
    (index: number) => {
      onChange(patterns.filter((_, i) => i !== index));
    },
    [onChange, patterns]
  );

  const handlePatternChange = useCallback(
    (index: number, value: string) => {
      onChange(patterns.map((p, i) => (i === index ? { pattern: value, regex: true } : p)));
    },
    [onChange, patterns]
  );

  // Compute validation errors for each regex. Patterns are compiled by the
  // backend with Go's regexp, so validation goes through the shared Go-syntax
  // helper instead of a raw JavaScript RegExp (which rejects "(?i)" etc.).
  const validationMap = useMemo(() => {
    const errorMap: Record<number, string | null> = {};
    patterns.forEach((p, idx) => {
      errorMap[idx] = goRegexValidationError(p.pattern);
    });
    return errorMap;
  }, [patterns]);

  return (
    <div className='space-y-4'>
      {patterns.length === 0 && (
        <div className='text-muted-foreground rounded-md border border-dashed p-6 text-center text-sm'>
          {t('system.retry.contentErrorDetection.patterns.emptyTitle')}
        </div>
      )}

      {patterns.map((item, index) => {
        const regexError = validationMap[index];

        return (
          <div key={index} className='rounded-md border p-4'>
            <div className='flex items-start gap-2'>
              <Input
                value={item.pattern}
                onChange={(e) => handlePatternChange(index, e.target.value)}
                placeholder={t('system.retry.contentErrorDetection.patterns.regexPlaceholder')}
                disabled={disabled}
                className={cn('font-mono text-sm', regexError && 'border-destructive focus-visible:ring-destructive')}
              />
              <Button
                type='button'
                variant='ghost'
                size='icon'
                onClick={() => handleRemovePattern(index)}
                disabled={disabled}
                aria-label={t('common.buttons.delete')}
                className='shrink-0'
              >
                <IconTrash className='h-4 w-4 text-red-500' />
              </Button>
            </div>
            {regexError && (
              <p className='text-destructive mt-2 flex items-center gap-1 text-xs'>
                <AlertCircle className='h-3.5 w-3.5 shrink-0' />
                <span>{t('system.retry.contentErrorDetection.patterns.invalidRegex', { error: regexError })}</span>
              </p>
            )}
          </div>
        );
      })}

      <Button type='button' variant='outline' className='w-full' onClick={handleAddPattern} disabled={disabled}>
        <IconPlus className='mr-2 h-4 w-4' />
        {t('system.retry.contentErrorDetection.patterns.add')}
      </Button>
    </div>
  );
}
