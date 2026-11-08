export interface KeepAliveConfig {
  enabled: boolean;
  intervalSeconds: number;
}

export interface KeepAliveSettings {
  streaming: KeepAliveConfig;
  nonStreaming: KeepAliveConfig;
  usingServerDefaults: boolean;
}

export interface UpdateKeepAliveInput {
  streaming: KeepAliveConfig;
  nonStreaming: KeepAliveConfig;
}

export function keepAliveValidation(input: UpdateKeepAliveInput): 'streamingInterval' | 'nonStreamingInterval' | null {
  if (!Number.isFinite(input.streaming.intervalSeconds) || input.streaming.intervalSeconds <= 0) {
    return 'streamingInterval';
  }
  if (!Number.isFinite(input.nonStreaming.intervalSeconds) || input.nonStreaming.intervalSeconds <= 0) {
    return 'nonStreamingInterval';
  }
  return null;
}
