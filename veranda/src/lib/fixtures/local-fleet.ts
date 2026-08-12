import type { LocalFleetSnapshot } from '../types';

export const localFleetFixture: LocalFleetSnapshot = {
  engineVersion: '0.1.0-dev',
  observedAt: '2026-08-12T12:00:00Z',
  currentYardName: 'default',
  owner: {
    id: 'local-host',
    yards: [
      {
        name: 'default',
        kind: 'container',
        state: 'RUNNING',
        projects: [
          { id: 'subyard', name: 'Subyard' },
          { id: 'docs', name: 'Documentation' }
        ]
      },
      {
        name: 'sandbox',
        kind: 'vm',
        state: 'STOPPED',
        projects: []
      }
    ]
  }
};
