import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import FleetApp from './FleetApp.svelte';
import type { LocalFleetSnapshot } from './types';

const snapshot: LocalFleetSnapshot = {
  engineVersion: '0.4.0',
  observedAt: '2026-08-12T10:00:00Z',
  currentYardName: 'default',
  owner: {
    id: 'owner-a',
    yards: [
      {
        name: 'default',
        kind: 'container',
        state: 'RUNNING',
        projects: [
          { id: 'alpha', name: 'Alpha' },
          { id: 'bravo', name: 'Bravo' }
        ]
      },
      { name: 'sandbox', kind: 'vm', state: 'STOPPED', projects: [] }
    ]
  }
};

describe('FleetApp', () => {
  it('selects the current yard and renders its plain project names', async () => {
    render(FleetApp, { loader: async () => snapshot });

    expect(await screen.findByRole('heading', { name: 'default' })).toBeInTheDocument();
    expect(screen.getByRole('list', { name: 'Projects in default' })).toHaveTextContent('Alpha');
    expect(screen.getByRole('list', { name: 'Projects in default' })).toHaveTextContent('Bravo');
    expect(screen.queryByText(/health|activity|agent/i)).not.toBeInTheDocument();
  });

  it('navigates between the owner and yards with native buttons', async () => {
    render(FleetApp, { loader: async () => snapshot });
    await screen.findByRole('heading', { name: 'default' });

    await fireEvent.click(screen.getByRole('button', { name: 'Show owner owner-a' }));
    expect(screen.getByRole('heading', { name: 'owner-a' })).toBeInTheDocument();
    expect(screen.getByText('2 yards')).toBeInTheDocument();

    await fireEvent.click(screen.getByRole('button', { name: 'Show yard sandbox' }));
    expect(screen.getByRole('heading', { name: 'sandbox' })).toBeInTheDocument();
    expect(screen.getByText('No projects in this yard')).toBeInTheDocument();
  });

  it('uses a singular yard count in the owner summary', async () => {
    const singleYard = {
      ...snapshot,
      owner: { ...snapshot.owner, yards: snapshot.owner.yards.slice(0, 1) }
    };
    render(FleetApp, { loader: async () => singleYard });
    await screen.findByRole('heading', { name: 'default' });

    await fireEvent.click(screen.getByRole('button', { name: 'Show owner owner-a' }));
    expect(screen.getByText('1 yard')).toBeInTheDocument();
  });

  it('shows an empty local owner without inventing a yard', async () => {
    const emptyOwner = { ...snapshot, owner: { ...snapshot.owner, yards: [] } };
    render(FleetApp, { loader: async () => emptyOwner });

    expect(await screen.findByRole('heading', { name: 'owner-a' })).toBeInTheDocument();
    expect(screen.getByText('No local yards found.')).toBeInTheDocument();
    expect(screen.getByText('0 yards')).toBeInTheDocument();
  });

  it('shows an actionable error and retries the loader', async () => {
    const loader = vi
      .fn<() => Promise<LocalFleetSnapshot>>()
      .mockRejectedValueOnce({ code: 'yard_not_found', message: 'Yard is not installed.' })
      .mockResolvedValueOnce(snapshot);

    render(FleetApp, { loader });
    expect(await screen.findByRole('alert')).toHaveTextContent('Yard is not installed.');

    await fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByRole('heading', { name: 'default' })).toBeInTheDocument();
    expect(loader).toHaveBeenCalledTimes(2);
  });

  it('announces loading while a refresh is pending', async () => {
    let resolveRefresh: (value: LocalFleetSnapshot) => void = () => undefined;
    const loader = vi
      .fn<() => Promise<LocalFleetSnapshot>>()
      .mockResolvedValueOnce(snapshot)
      .mockImplementationOnce(
        () => new Promise<LocalFleetSnapshot>((resolve) => (resolveRefresh = resolve))
      );

    render(FleetApp, { loader });
    await screen.findByRole('heading', { name: 'default' });
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh local fleet' }));

    expect(screen.getByRole('status')).toHaveTextContent('Refreshing local fleet');
    resolveRefresh(snapshot);
    await waitFor(() => expect(screen.queryByText('Refreshing local fleet')).not.toBeInTheDocument());
  });
});
