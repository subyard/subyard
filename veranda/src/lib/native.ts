import { invoke } from '@tauri-apps/api/core';
import type { FleetLoader, LocalFleetSnapshot } from './types';
export const loadLocalFleet: FleetLoader = async (): Promise<LocalFleetSnapshot> => {
    if (import.meta.env.DEV && import.meta.env.VITE_VERANDA_FIXTURE === 'local') {
        const { localFleetFixture } = await import('./fixtures/local-fleet');
        return localFleetFixture;
    }
    return invoke<LocalFleetSnapshot>('load_local_fleet');
};
import { listen } from '@tauri-apps/api/event';
import type { VerandaApi, VerandaEvent } from './types';
const fixtureMode = import.meta.env.DEV && import.meta.env.VITE_VERANDA_FIXTURE === 'local';
type FrontendDiagnostic = 'boundary' | 'error' | 'unhandled_rejection';
const reportedDiagnostics = new Set<FrontendDiagnostic>();
export function reportFrontendDiagnostic(event: FrontendDiagnostic): void {
    if (fixtureMode || reportedDiagnostics.has(event)) return;
    reportedDiagnostics.add(event);
    // Only a fixed event code crosses IPC; native logging is opt-in and bounded.
    void invoke('frontend_diagnostic', { event }).catch(() => {});
}
export const nativeApi: VerandaApi = {
    markReady: (fleet) => fixtureMode ? Promise.resolve() : invoke('frontend_ready', { fleet }),
    listConnections: () => fixtureMode ? Promise.resolve([]) : invoke('list_connections'),
    loadFleet: (args) => fixtureMode ? loadLocalFleet() : invoke('load_fleet', args),
    assessConnection: (args) => invoke('assess_connection', args),
    cancelAssessment: (args) => invoke('cancel_assessment', args),
    connectRemote: (args) => invoke('connect_remote', args),
    repairConnection: (args) => invoke('repair_connection', args),
    assessRemoval: (args) => invoke('assess_removal', args),
    removeConnection: (args) => invoke('remove_connection', args),
    loadYard: (args) => fixtureMode ? Promise.resolve({ profiles: [], settings: [], diagnostics: [], capabilities: [] }) : invoke('load_yard', args),
    loadHost: (args) => fixtureMode ? Promise.resolve({ settings: [], diagnostics: [], sync: null, capabilities: [] }) : invoke('load_host', args),
    planOperation: (args) => invoke('plan_operation', args),
    executeOperation: (args) => invoke('execute_operation', args),
    discardOperation: (args) => invoke('discard_operation', args),
    cancelOperation: (args) => invoke('cancel_operation', args),
    launchSession: (args) => invoke('launch_session', args),
    shellCommand: (args) => invoke('shell_command', args),
    listen: (handler) => fixtureMode ? Promise.resolve(() => { }) : listen<VerandaEvent>('veranda:event', (event) => handler(event.payload))
};
