import { type ReactNode, useRef, useState } from 'react';
import { Info, Loader2 } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { useAuthStore } from '@/stores/authStore';
import { usePermissions } from '@/hooks/usePermissions';
import { Alert, AlertDescription } from '@/components/ui/alert';
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from '@/components/ui/alert-dialog';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { Separator } from '@/components/ui/separator';
import { Switch } from '@/components/ui/switch';
import {
  keepAliveValidation,
  type KeepAliveConfig,
  type KeepAliveSettings as KeepAliveSettingsType,
  type UpdateKeepAliveInput,
} from '../data/keep-alive';
import { useKeepAliveSettings, useSaveKeepAliveSettings } from '../data/keep-alive-api';

export function KeepAliveSettings() {
  const userID = useAuthStore((state) => state.auth.user?.id ?? 'signed-out');
  return <KeepAliveSettingsForm key={userID} />;
}

function KeepAliveSettingsForm() {
  const { t } = useTranslation();
  const { hasSystemScope } = usePermissions();
  const query = useKeepAliveSettings();
  const save = useSaveKeepAliveSettings();

  const [draftStreaming, setDraftStreaming] = useState<Partial<KeepAliveConfig>>({});
  const [draftNonStreaming, setDraftNonStreaming] = useState<Partial<KeepAliveConfig>>({});
  const [isSaveConfirmOpen, setIsSaveConfirmOpen] = useState(false);
  const [isResetConfirmOpen, setIsResetConfirmOpen] = useState(false);

  const serverDefaultsRef = useRef<{ streaming: KeepAliveConfig; nonStreaming: KeepAliveConfig } | null>(null);

  const canWrite = hasSystemScope('write_settings');

  if (!hasSystemScope('read_settings')) {
    return (
      <Alert>
        <AlertDescription>{t('common.routeGuard.noPermission')}</AlertDescription>
      </Alert>
    );
  }

  if (query.isPending) {
    return (
      <div className='flex items-center gap-2 p-6'>
        <Loader2 className='h-5 w-5 animate-spin' />
        {t('common.loading')}
      </div>
    );
  }

  if (query.isError || !query.data) {
    return (
      <Alert variant='destructive'>
        <AlertDescription className='flex items-center justify-between'>
          <span>{t('system.keepAlive.loadFailed')}</span>
          <Button variant='outline' size='sm' onClick={() => void query.refetch()}>
            {t('system.keepAlive.retry')}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  const saved: KeepAliveSettingsType = query.data;

  // Track known server defaults when returned
  if (saved.usingServerDefaults) {
    serverDefaultsRef.current = {
      streaming: { ...saved.streaming },
      nonStreaming: { ...saved.nonStreaming },
    };
  }

  const currentStreaming: KeepAliveConfig = {
    enabled: draftStreaming.enabled ?? saved.streaming.enabled,
    intervalSeconds: draftStreaming.intervalSeconds ?? saved.streaming.intervalSeconds,
  };

  const currentNonStreaming: KeepAliveConfig = {
    enabled: draftNonStreaming.enabled ?? saved.nonStreaming.enabled,
    intervalSeconds: draftNonStreaming.intervalSeconds ?? saved.nonStreaming.intervalSeconds,
  };

  const input: UpdateKeepAliveInput = {
    streaming: currentStreaming,
    nonStreaming: currentNonStreaming,
  };

  const issue = keepAliveValidation(input);
  const disabled = !canWrite || save.isPending;
  const dirty = Object.keys(draftStreaming).length > 0 || Object.keys(draftNonStreaming).length > 0;

  const clearDraft = () => {
    setDraftStreaming({});
    setDraftNonStreaming({});
  };

  const defaultsToCompare = serverDefaultsRef.current;

  return (
    <Card data-testid='keep-alive-settings'>
      <CardHeader>
        <div className='flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between'>
          <div>
            <CardTitle>{t('system.keepAlive.title')}</CardTitle>
            <CardDescription className='mt-1'>{t('system.keepAlive.description')}</CardDescription>
          </div>
          <div>
            {saved.usingServerDefaults ? (
              <Badge variant='secondary' className='font-normal'>
                {t('system.keepAlive.statusUsingServerDefaults')}
              </Badge>
            ) : (
              <Badge variant='outline' className='font-normal'>
                {t('system.keepAlive.statusCustom')}
              </Badge>
            )}
          </div>
        </div>
      </CardHeader>
      <CardContent>
        <form
          className='space-y-6'
          onSubmit={(event) => {
            event.preventDefault();
            if (!disabled && !issue && dirty) {
              setIsSaveConfirmOpen(true);
            }
          }}
        >
          <p className='text-muted-foreground text-sm'>
            {t(saved.usingServerDefaults ? 'system.keepAlive.serverDefaultsNotice' : 'system.keepAlive.savedOverrideNotice')}
          </p>

          {!canWrite && (
            <Alert>
              <AlertDescription>{t('system.keepAlive.readOnly')}</AlertDescription>
            </Alert>
          )}

          <fieldset disabled={disabled} className='space-y-6'>
            {/* Streaming Heartbeat Section */}
            <KeepAliveSection
              idPrefix='streaming'
              title={t('system.keepAlive.streaming.title')}
              description={t('system.keepAlive.streaming.description')}
              intervalLabel={t('system.keepAlive.streaming.intervalLabel')}
              intervalHelp={t('system.keepAlive.streaming.intervalHelp')}
              config={currentStreaming}
              disabled={disabled}
              onEnabledChange={(checked) => setDraftStreaming((prev) => ({ ...prev, enabled: checked }))}
              onIntervalChange={(seconds) => setDraftStreaming((prev) => ({ ...prev, intervalSeconds: seconds }))}
            />

            {/* Non-Streaming Keep-Alive Section */}
            <KeepAliveSection
              idPrefix='non-streaming'
              title={t('system.keepAlive.nonStreaming.title')}
              description={t('system.keepAlive.nonStreaming.description')}
              warning={t('system.keepAlive.nonStreaming.warning')}
              intervalLabel={t('system.keepAlive.nonStreaming.intervalLabel')}
              intervalHelp={t('system.keepAlive.nonStreaming.intervalHelp')}
              config={currentNonStreaming}
              disabled={disabled}
              onEnabledChange={(checked) => setDraftNonStreaming((prev) => ({ ...prev, enabled: checked }))}
              onIntervalChange={(seconds) => setDraftNonStreaming((prev) => ({ ...prev, intervalSeconds: seconds }))}
            />

            {/* Server Defaults Comparison Reference */}
            {defaultsToCompare && (
              <div className='bg-muted/40 space-y-1 rounded-lg border border-dashed p-3 text-sm'>
                <div className='text-muted-foreground flex items-center gap-2 font-medium'>
                  <Info className='h-4 w-4' />
                  <span>{t('system.keepAlive.serverDefaultsComparison')}</span>
                </div>
                <div className='text-muted-foreground text-xs'>
                  {t('system.keepAlive.serverDefaultsComparisonDetail', {
                    streaming: defaultsToCompare.streaming.enabled
                      ? t('system.keepAlive.statusEnabled', { interval: defaultsToCompare.streaming.intervalSeconds })
                      : t('system.keepAlive.statusDisabled', { interval: defaultsToCompare.streaming.intervalSeconds }),
                    nonStreaming: defaultsToCompare.nonStreaming.enabled
                      ? t('system.keepAlive.statusEnabled', { interval: defaultsToCompare.nonStreaming.intervalSeconds })
                      : t('system.keepAlive.statusDisabled', { interval: defaultsToCompare.nonStreaming.intervalSeconds }),
                  })}
                </div>
              </div>
            )}
          </fieldset>

          {issue && (
            <p role='alert' className='text-destructive text-sm font-medium'>
              {t(`system.keepAlive.validation.${issue}`)}
            </p>
          )}

          <Separator />

          <div className='flex flex-wrap items-center justify-end gap-2'>
            {/* Reset to server defaults AlertDialog */}
            <AlertDialog open={isResetConfirmOpen} onOpenChange={setIsResetConfirmOpen}>
              <AlertDialogTrigger asChild>
                <Button type='button' variant='outline' disabled={disabled || saved.usingServerDefaults} data-testid='keep-alive-reset'>
                  {t('system.keepAlive.reset')}
                </Button>
              </AlertDialogTrigger>
              <AlertDialogContent>
                <AlertDialogHeader>
                  <AlertDialogTitle>{t('system.keepAlive.resetConfirmTitle')}</AlertDialogTitle>
                  <AlertDialogDescription>{t('system.keepAlive.resetConfirmDescription')}</AlertDialogDescription>
                </AlertDialogHeader>
                <AlertDialogFooter>
                  <AlertDialogCancel>{t('common.cancel')}</AlertDialogCancel>
                  <AlertDialogAction
                    data-testid='keep-alive-reset-confirm'
                    onClick={() => {
                      save.mutate(null, {
                        onSuccess: () => {
                          clearDraft();
                          setIsResetConfirmOpen(false);
                        },
                      });
                    }}
                  >
                    {t('common.confirm')}
                  </AlertDialogAction>
                </AlertDialogFooter>
              </AlertDialogContent>
            </AlertDialog>

            {/* Discard draft button */}
            <Button type='button' variant='outline' disabled={disabled || !dirty} data-testid='keep-alive-discard' onClick={clearDraft}>
              {t('system.keepAlive.discard')}
            </Button>

            {/* Save confirmation AlertDialog */}
            <AlertDialog open={isSaveConfirmOpen} onOpenChange={setIsSaveConfirmOpen}>
              <AlertDialogTrigger asChild>
                <Button data-testid='keep-alive-save' type='submit' disabled={disabled || !!issue || !dirty}>
                  {save.isPending && <Loader2 className='mr-2 h-4 w-4 animate-spin' />}
                  {t(save.isPending ? 'system.buttons.saving' : 'system.buttons.save')}
                </Button>
              </AlertDialogTrigger>
              <AlertDialogContent>
                <AlertDialogHeader>
                  <AlertDialogTitle>{t('system.keepAlive.saveConfirmTitle')}</AlertDialogTitle>
                  <AlertDialogDescription>{t('system.keepAlive.saveConfirmDescription')}</AlertDialogDescription>
                </AlertDialogHeader>
                <AlertDialogFooter>
                  <AlertDialogCancel>{t('common.cancel')}</AlertDialogCancel>
                  <AlertDialogAction
                    data-testid='keep-alive-save-confirm'
                    onClick={() => {
                      save.mutate(input, {
                        onSuccess: () => {
                          clearDraft();
                          setIsSaveConfirmOpen(false);
                        },
                      });
                    }}
                  >
                    {t('common.confirm')}
                  </AlertDialogAction>
                </AlertDialogFooter>
              </AlertDialogContent>
            </AlertDialog>
          </div>
        </form>
      </CardContent>
    </Card>
  );
}

interface KeepAliveSectionProps {
  idPrefix: string;
  title: ReactNode;
  description: ReactNode;
  warning?: ReactNode;
  intervalLabel: ReactNode;
  intervalHelp: ReactNode;
  config: KeepAliveConfig;
  disabled?: boolean;
  onEnabledChange: (enabled: boolean) => void;
  onIntervalChange: (intervalSeconds: number) => void;
}

function KeepAliveSection({
  idPrefix,
  title,
  description,
  warning,
  intervalLabel,
  intervalHelp,
  config,
  disabled = false,
  onEnabledChange,
  onIntervalChange,
}: KeepAliveSectionProps) {
  return (
    <div className='space-y-4 rounded-lg border p-4'>
      <div className='flex items-start justify-between gap-4'>
        <div className='space-y-1'>
          <Label htmlFor={`keep-alive-${idPrefix}-enabled`} className='text-base font-medium'>
            {title}
          </Label>
          <p className='text-muted-foreground text-sm'>{description}</p>
          {warning && <p className='text-xs font-medium text-amber-600 dark:text-amber-400'>{warning}</p>}
        </div>
        <Switch
          id={`keep-alive-${idPrefix}-enabled`}
          data-testid={`keep-alive-${idPrefix}-switch`}
          checked={config.enabled}
          onCheckedChange={onEnabledChange}
        />
      </div>

      <div className='space-y-2 pt-2'>
        <Label htmlFor={`keep-alive-${idPrefix}-interval`} className={!config.enabled ? 'text-muted-foreground' : ''}>
          {intervalLabel}
        </Label>
        <div className='max-w-xs'>
          <Input
            id={`keep-alive-${idPrefix}-interval`}
            data-testid={`keep-alive-${idPrefix}-interval`}
            type='number'
            min='0.001'
            step='any'
            required
            disabled={disabled || !config.enabled}
            value={Number.isNaN(config.intervalSeconds) ? '' : config.intervalSeconds}
            onChange={(e) => {
              const val = e.target.valueAsNumber;
              onIntervalChange(Number.isNaN(val) ? 0 : val);
            }}
          />
        </div>
        <p className='text-muted-foreground text-xs'>{intervalHelp}</p>
      </div>
    </div>
  );
}
