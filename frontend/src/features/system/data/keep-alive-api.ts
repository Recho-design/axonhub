import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { graphqlRequest } from '@/gql/graphql';
import { toast } from 'sonner';
import { useAuthStore } from '@/stores/authStore';
import i18n from '@/lib/i18n';
import { useErrorHandler } from '@/hooks/use-error-handler';
import { usePermissions } from '@/hooks/usePermissions';
import type { KeepAliveSettings, UpdateKeepAliveInput } from './keep-alive';

const query = `
  query KeepAliveSettings {
    keepAliveSettings {
      usingServerDefaults
      streaming {
        enabled
        intervalSeconds
      }
      nonStreaming {
        enabled
        intervalSeconds
      }
    }
  }
`;

export function useKeepAliveSettings() {
  const userID = useAuthStore((state) => state.auth.user?.id ?? null);
  const { hasSystemScope } = usePermissions();
  const canRead = hasSystemScope('read_settings');
  return useQuery({
    queryKey: ['keepAliveSettings', userID, canRead],
    enabled: userID !== null && canRead,
    queryFn: async () => {
      const data = await graphqlRequest<{ keepAliveSettings: KeepAliveSettings }>(query);
      return data.keepAliveSettings;
    },
  });
}

export function useSaveKeepAliveSettings() {
  const client = useQueryClient();
  const { handleError } = useErrorHandler();
  return useMutation({
    mutationFn: async (input: UpdateKeepAliveInput | null) => {
      if (input === null) {
        await graphqlRequest(`mutation ResetKeepAliveSettings { resetKeepAliveSettings }`);
      } else {
        await graphqlRequest(
          `
          mutation UpdateKeepAliveSettings($input: UpdateKeepAliveInput!) {
            updateKeepAliveSettings(input: $input)
          }
        `,
          { input }
        );
      }
    },
    onSuccess: async (_data, variables) => {
      await client.invalidateQueries({ queryKey: ['keepAliveSettings'] });
      toast.success(i18n.t(variables === null ? 'system.keepAlive.resetSuccess' : 'system.keepAlive.saved'));
    },
    onError: (error) => handleError(error, i18n.t('system.keepAlive.saveFailed')),
  });
}
