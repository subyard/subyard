import { invoke } from '@tauri-apps/api/core';
import type { FleetLoader, LocalFleetSnapshot } from './types';

export const loadLocalFleet: FleetLoader = async (): Promise<LocalFleetSnapshot> => {
  if (import.meta.env.DEV && import.meta.env.VITE_VERANDA_FIXTURE === 'local') {
    const { localFleetFixture } = await import('./fixtures/local-fleet');
    return localFleetFixture;
  }

  return invoke<LocalFleetSnapshot>('load_local_fleet');
};
