'use client';

import { forwardRef, useCallback, useEffect, useRef, useState, useMemo } from 'react';
import { Check } from 'lucide-react';
import { X } from 'lucide-react';
import { cn } from '@/lib/utils';
import { TruncatedText } from '@/components/truncated-text';
import { Popover, PopoverContent, PopoverTrigger } from './popover';

const MAX_DISPLAY = 100;

interface TagsAutocompleteInputProps {
  value: string[];
  onChange: (tags: string[]) => void;
  placeholder?: string;
  className?: string;
  suggestions?: string[];
  isLoading?: boolean;
  strict?: boolean;
  emptyMessage?: string;
}

export const TagsAutocompleteInput = forwardRef<HTMLDivElement, TagsAutocompleteInputProps>(
  ({ value = [], onChange, placeholder, className, suggestions = [], isLoading, strict = false, emptyMessage }, ref) => {
    const [inputValue, setInputValue] = useState('');
    const [open, setOpen] = useState(false);
    const [isComposing, setIsComposing] = useState(false);
    const containerRef = useRef<HTMLDivElement>(null);
    const inputRef = useRef<HTMLInputElement>(null);

    // Filter suggestions based on input and not already selected (capped for performance)
    const filteredSuggestions = useMemo(() => {
      const result: string[] = [];
      const q = inputValue.trim().toLowerCase();
      for (const s of suggestions) {
        if (value.includes(s)) continue;
        if (q && !s.toLowerCase().includes(q)) continue;
        result.push(s);
        if (result.length >= MAX_DISPLAY) break;
      }
      return result;
    }, [inputValue, suggestions, value]);

    /** In strict mode, only an exact (case-insensitive) suggestion match is
     * accepted on commit; free-form input is discarded. */
    const resolveMatch = useCallback(
      (input: string): string | null => {
        const trimmed = input.trim();
        if (!trimmed) return null;
        if (!strict) return trimmed;
        return suggestions.find((s) => s.toLowerCase() === trimmed.toLowerCase()) ?? null;
      },
      [strict, suggestions]
    );

    // The popover can open when there are suggestions to show, or in strict
    // mode when an empty-state message replaces the free-form "add" hint.
    const canShowPopover = suggestions.length > 0 || (strict && Boolean(emptyMessage));

    const handleInputChange = (e: React.ChangeEvent<HTMLInputElement>) => {
      setInputValue(e.target.value);
      if (e.target.value && !open && canShowPopover) {
        setOpen(true);
      } else if (!e.target.value) {
        setOpen(false);
      }
    };

    const handleKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
      if (isComposing) return;

      if (e.key === 'Enter' || e.key === ',') {
        e.preventDefault();
        const matched = resolveMatch(inputValue);
        if (matched && !value.includes(matched)) {
          onChange([...value, matched]);
        }
        setInputValue('');
        setOpen(false);
      } else if (e.key === 'Backspace' && !inputValue && value.length > 0) {
        onChange(value.slice(0, -1));
      } else if (e.key === 'Escape') {
        setOpen(false);
      }
    };

    const removeTag = (tagToRemove: string) => {
      onChange(value.filter((tag) => tag !== tagToRemove));
    };

    const handleSelectSuggestion = (suggestion: string) => {
      if (!value.includes(suggestion)) {
        onChange([...value, suggestion]);
      }
      setInputValue('');
      setOpen(false);
      inputRef.current?.focus();
    };

    const handleInputBlur = () => {
      if (isComposing) return;
      const matched = resolveMatch(inputValue);
      if (matched && !value.includes(matched)) {
        onChange([...value, matched]);
      }
      setInputValue('');
      setOpen(false);
    };

    // Focus input when clicking on the container
    const handleContainerClick = () => {
      inputRef.current?.focus();
      if (canShowPopover && inputValue) {
        setOpen(true);
      }
    };

    // Close popover when clicking outside
    useEffect(() => {
      const handleClickOutside = (e: MouseEvent) => {
        const target = e.target as HTMLElement;
        if (!target.closest('[data-tags-input-container]')) {
          setOpen(false);
        }
      };

      document.addEventListener('mousedown', handleClickOutside);
      return () => document.removeEventListener('mousedown', handleClickOutside);
    }, []);

    const hasPopoverContent =
      isLoading ||
      filteredSuggestions.length > 0 ||
      (Boolean(inputValue.trim()) && (!strict || Boolean(emptyMessage)));

    return (
      <div
        ref={containerRef}
        data-tags-input-container
        className={cn(
          'border-input bg-background has-[input:focus-visible]:border-ring has-[input:focus-visible]:outline-foreground flex min-h-10 w-full flex-wrap gap-1 rounded-md border px-3 py-2 text-sm has-[input:focus-visible]:outline-2 has-[input:focus-visible]:outline-offset-1',
          className
        )}
        onClick={handleContainerClick}
      >
        {value.map((tag) => (
          <div key={tag} className='bg-secondary text-secondary-foreground flex items-center gap-1 rounded-sm px-2 py-0.5'>
            <span className='text-xs'>{tag}</span>
            <button
              type='button'
              onClick={() => removeTag(tag)}
              className='text-secondary-foreground/80 hover:text-secondary-foreground focus-visible:outline-foreground rounded-sm focus-visible:outline-2 focus-visible:outline-offset-1'
              aria-label={`Remove ${tag} tag`}
            >
              <X className='h-3 w-3' />
            </button>
          </div>
        ))}
        <Popover open={open} onOpenChange={setOpen} modal={false}>
          <PopoverTrigger asChild>
            <input
              ref={inputRef}
              type='text'
              value={inputValue}
              onChange={handleInputChange}
              onKeyDown={handleKeyDown}
              onBlur={handleInputBlur}
              onFocus={() => {
                if (canShowPopover && inputValue) {
                  setOpen(true);
                }
              }}
              onCompositionStart={() => setIsComposing(true)}
              onCompositionEnd={() => setIsComposing(false)}
              placeholder={value.length === 0 ? placeholder : ''}
              className='placeholder:text-muted-foreground min-w-[80px] flex-1 bg-transparent outline-none'
            />
          </PopoverTrigger>
          {hasPopoverContent && (
            <PopoverContent
              className='w-[var(--radix-popover-trigger-width)] max-w-[var(--radix-popover-trigger-width)] p-0'
              align='start'
              onOpenAutoFocus={(e) => e.preventDefault()}
              container={containerRef.current ?? undefined}
            >
              <div className='max-h-[200px] overflow-y-auto p-1'>
                {isLoading ? (
                  <div className='text-muted-foreground p-2 text-sm'>Loading...</div>
                ) : filteredSuggestions.length > 0 ? (
                  filteredSuggestions.map((suggestion) => (
                    <div
                      key={suggestion}
                      className='hover:bg-accent flex cursor-pointer items-center gap-2 rounded-md px-2 py-1.5 text-sm'
                      onMouseDown={(e) => {
                        e.preventDefault();
                        handleSelectSuggestion(suggestion);
                      }}
                    >
                      <Check className='text-muted-foreground h-4 w-4 shrink-0' />
                      <TruncatedText className='flex-1'>{suggestion}</TruncatedText>
                    </div>
                  ))
                ) : inputValue.trim() ? (
                  strict ? (
                    emptyMessage ? (
                      <div className='text-muted-foreground p-2 text-sm'>{emptyMessage}</div>
                    ) : null
                  ) : (
                    <div className='text-muted-foreground p-2 text-sm'>Press Enter to add &quot;{inputValue}&quot;</div>
                  )
                ) : null}
              </div>
            </PopoverContent>
          )}
        </Popover>
      </div>
    );
  }
);

TagsAutocompleteInput.displayName = 'TagsAutocompleteInput';
