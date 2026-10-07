import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import Page from './+page.svelte';

const mocks = vi.hoisted(() => ({ fleet: vi.fn(), diagnostic: vi.fn() }));
vi.mock('$lib/FleetApp.svelte', () => ({ default: mocks.fleet }));
vi.mock('$lib/native', () => ({ reportFrontendDiagnostic: mocks.diagnostic }));

beforeEach(() => { mocks.fleet.mockReset(); mocks.diagnostic.mockClear(); });

describe('page error boundary', () => {
    it('replaces a render failure with a fixed actionable fallback and retries the fleet', async () => {
        mocks.fleet.mockImplementationOnce(() => { throw new Error('private exception detail'); });
        render(Page);
        expect(await screen.findByRole('alert')).toHaveTextContent('Veranda could not display the fleet');
        expect(document.body).not.toHaveTextContent('private exception detail');
        expect(mocks.diagnostic).toHaveBeenCalledWith('boundary');
        await fireEvent.click(screen.getByRole('button', { name: 'Reload fleet' }));
        await waitFor(() => expect(screen.queryByRole('alert')).not.toBeInTheDocument());
        expect(mocks.fleet).toHaveBeenCalledTimes(2);
    });

    it('reports only fixed global event codes and removes its listeners on unmount', () => {
        const page = render(Page);
        window.dispatchEvent(new ErrorEvent('error', { message: 'private exception detail', filename: 'private URL' }));
        window.dispatchEvent(new Event('unhandledrejection'));
        expect(mocks.diagnostic.mock.calls).toEqual([['error'], ['unhandled_rejection']]);
        page.unmount();
        window.dispatchEvent(new Event('error'));
        window.dispatchEvent(new Event('unhandledrejection'));
        expect(mocks.diagnostic).toHaveBeenCalledTimes(2);
    });
});
