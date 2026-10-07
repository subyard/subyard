import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { tick } from 'svelte';
import { describe, expect, it, vi } from 'vitest';
import FleetApp from './FleetApp.svelte';
import type { LocalFleetSnapshot } from './types';
const snapshot: LocalFleetSnapshot = {
    engineVersion: '0.4.0',
    capabilities: ['session-prepare-v1'],
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
            .mockImplementationOnce(() => new Promise<LocalFleetSnapshot>((resolve) => (resolveRefresh = resolve)));
        render(FleetApp, { loader });
        await screen.findByRole('heading', { name: 'default' });
        await fireEvent.click(screen.getByRole('button', { name: 'Refresh local fleet' }));
        expect(screen.getByRole('status')).toHaveTextContent('Refreshing local fleet');
        resolveRefresh(snapshot);
        await waitFor(() => expect(screen.queryByText('Refreshing local fleet')).not.toBeInTheDocument());
    });
});
import type { VerandaApi, VerandaEvent } from './types';
function mockApi(overrides: Partial<VerandaApi> = {}): VerandaApi {
    return {
        listConnections: vi.fn().mockResolvedValue([]), loadFleet: vi.fn().mockResolvedValue(snapshot),
        loadYard: vi.fn().mockResolvedValue({ profiles: [], settings: [], diagnostics: [], capabilities: [] }),
        loadHost: vi.fn().mockResolvedValue({ settings: [], diagnostics: [], sync: null, capabilities: [] }),
        assessConnection: vi.fn().mockResolvedValue({ assessmentId: 'assessment', destination: 'user@host', fingerprint: 'SHA256:verified', confirmation: 'prompt-default-no', consequences: ['Save this pinned public host key.'] }),
        connectRemote: vi.fn().mockResolvedValue({ id: 'remote', hostId: 'owner-b', destination: 'user@host', authLabel: 'SSH agent' }),
        cancelAssessment: vi.fn().mockResolvedValue(undefined), repairConnection: vi.fn(), assessRemoval: vi.fn(), removeConnection: vi.fn(), planOperation: vi.fn(), executeOperation: vi.fn(), discardOperation: vi.fn(), cancelOperation: vi.fn(), launchSession: vi.fn(), shellCommand: vi.fn(), listen: vi.fn().mockResolvedValue(() => { }), ...overrides
    };
}
describe('native fleet flows', () => {
    it('distinguishes local and SSH owners with the same ID by accessible destination and selects their connection IDs', async () => {
        localStorage.clear();
        const connection = {id: 'ssh-owner-a', hostId: 'owner-a', destination: 'operator@example.test', authLabel: 'SSH agent'};
        const api = mockApi({listConnections: vi.fn().mockResolvedValue([connection])});
        render(FleetApp, {api});
        await screen.findByRole('heading', {name: 'default'});
        const local = screen.getByRole('button', {name: 'Show owner owner-a'});
        const remote = screen.getByRole('button', {name: 'Show owner owner-a at operator@example.test'});
        expect(local).not.toBe(remote);
        await fireEvent.click(remote);
        await waitFor(() => expect(api.loadHost).toHaveBeenLastCalledWith({connectionId: connection.id}));
        expect(remote).toHaveAttribute('aria-pressed', 'true');
        expect(local).toHaveAttribute('aria-pressed', 'false');
        await fireEvent.click(local);
        await waitFor(() => expect(api.loadHost).toHaveBeenLastCalledWith({connectionId: null}));
        expect(local).toHaveAttribute('aria-pressed', 'true');
        expect(remote).toHaveAttribute('aria-pressed', 'false');
    });
    it('copies a prepared shell command independently and clears commands on selection changes', async () => {
        localStorage.clear();
        let resolveLate: (value: {command: string}) => void = () => {};
        const api = mockApi({shellCommand: vi.fn().mockResolvedValueOnce({command: 'yard -Y default shell'}).mockImplementationOnce(() => new Promise(resolve => {resolveLate = resolve;}))});
        render(FleetApp, {api});
        await screen.findByRole('heading', {name: 'default'});
        await fireEvent.click(screen.getByRole('button', {name: 'Copy shell command'}));
        expect(await screen.findByLabelText('Shell command')).toHaveValue('yard -Y default shell');
        expect(screen.getByLabelText('Shell command')).toHaveAttribute('readonly');
        expect(screen.getByText('Select the command below and copy it manually.')).toBeInTheDocument();
        expect(api.shellCommand).toHaveBeenCalledWith({connectionId: null, yard: 'default', projectId: null});
        expect(api.launchSession).not.toHaveBeenCalled();
        await fireEvent.click(screen.getByRole('button', {name: 'Show yard sandbox'}));
        expect(screen.queryByLabelText('Shell command')).not.toBeInTheDocument();
        await fireEvent.click(screen.getByRole('button', {name: 'Copy shell command'}));
        await fireEvent.click(screen.getByRole('button', {name: 'Show yard default'}));
        resolveLate({command: 'yard -Y sandbox shell'});
        await waitFor(() => expect(screen.getByRole('button', {name: 'Copy shell command'})).toBeEnabled());
        expect(screen.queryByLabelText('Shell command')).not.toBeInTheDocument();
    });
    it('makes the local fleet ready without waiting for remote SSH and preserves manual selection on late arrival', async () => {
        localStorage.setItem('veranda-selection', JSON.stringify({connectionId: 'remote', yardName: 'default', tab: 'Projects'}));
        let resolveRemote: (fleet: LocalFleetSnapshot) => void = () => {};
        const api = mockApi({
            listConnections: vi.fn().mockResolvedValue([{id: 'remote', hostId: 'owner-b', destination: 'user@host', authLabel: 'SSH agent'}]),
            loadFleet: vi.fn().mockImplementation(({connectionId}) => connectionId === null ? Promise.resolve(snapshot) : new Promise(resolve => {resolveRemote = resolve;})),
            markReady: vi.fn().mockResolvedValue(undefined)
        });
        render(FleetApp, {api});
        expect(await screen.findByRole('heading', {name: 'default'})).toBeInTheDocument();
        expect(screen.getByRole('list', {name: 'Projects in default'})).toHaveTextContent('Alpha');
        expect(screen.getByRole('button', {name: 'Show owner owner-b at user@host'})).toHaveTextContent('connecting');
        await waitFor(() => expect(api.markReady).toHaveBeenCalledOnce());
        expect(api.markReady).toHaveBeenCalledWith({owners: 1, yards: snapshot.owner.yards.length,
            projects: snapshot.owner.yards.reduce((sum, yard) => sum + yard.projects.length, 0)});
        await fireEvent.click(screen.getByRole('button', {name: 'Show yard sandbox'}));
        resolveRemote({...snapshot, owner: {...snapshot.owner, id: 'owner-b'}});
        await waitFor(() => expect(screen.getByRole('button', {name: 'Show owner owner-b at user@host'})).toHaveTextContent('connected'));
        expect(screen.getByRole('heading', {name: 'sandbox'})).toBeInTheDocument();
    });
    it('shows an ordinary local error while a saved remote is still pending', async () => {
        localStorage.clear();
        const api = mockApi({
            listConnections: vi.fn().mockResolvedValue([{id: 'remote', hostId: 'owner-b', destination: 'user@host', authLabel: 'SSH agent'}]),
            loadFleet: vi.fn().mockImplementation(({connectionId}) => connectionId === null ? Promise.reject({message: 'This platform supports remote owner hosts. Add an SSH connection.'}) : new Promise(() => {})),
            markReady: vi.fn().mockResolvedValue(undefined)
        });
        render(FleetApp, {api});
        expect(await screen.findByRole('alert')).toHaveTextContent('Add an SSH connection.');
        expect(screen.getByRole('button', {name: /Connect remote host/})).toBeEnabled();
        await waitFor(() => expect(api.markReady).toHaveBeenCalledOnce());
    });
    it('opens the SSH form on a remote-only first launch without assessing or mutating', async () => {
        localStorage.clear();
        const api = mockApi({
            loadFleet: vi.fn().mockRejectedValue({code: 'remote_only', message: 'This platform supports remote owner hosts. Add an SSH connection.'}),
            markReady: vi.fn().mockResolvedValue(undefined)
        });
        render(FleetApp, {api});
        expect(await screen.findByLabelText('SSH destination')).toHaveValue('');
        expect(screen.getByRole('button', {name: 'Test connection'})).toBeEnabled();
        expect(screen.queryByText('Local owner host')).not.toBeInTheDocument();
        expect(screen.queryByRole('alert')).not.toBeInTheDocument();
        expect(screen.queryByRole('heading', {name: 'Fleet unavailable'})).not.toBeInTheDocument();
        await waitFor(() => expect(api.markReady).toHaveBeenCalledOnce());
        expect(api.assessConnection).not.toHaveBeenCalled();
        expect(api.connectRemote).not.toHaveBeenCalled();
        expect(api.planOperation).not.toHaveBeenCalled();
        expect(api.executeOperation).not.toHaveBeenCalled();
        expect(api.launchSession).not.toHaveBeenCalled();
    });
    it('omits an unsupported local row while remote SSH is pending and respects a later manual selection', async () => {
        localStorage.setItem('veranda-selection', JSON.stringify({connectionId: 'remote', yardName: 'sandbox', tab: 'Projects'}));
        let resolveRemote: (fleet: LocalFleetSnapshot) => void = () => {};
        const api = mockApi({
            listConnections: vi.fn().mockResolvedValue([{id: 'remote', hostId: 'owner-b', destination: 'user@host', authLabel: 'SSH agent'}]),
            loadFleet: vi.fn().mockImplementation(({connectionId}) => connectionId === null
                ? Promise.reject({code: 'remote_only', message: 'This platform supports remote owner hosts. Add an SSH connection.'})
                : new Promise(resolve => {resolveRemote = resolve;})),
            markReady: vi.fn().mockResolvedValue(undefined)
        });
        render(FleetApp, {api});
        const remote = await screen.findByRole('button', {name: 'Show owner owner-b at user@host'});
        expect(remote).toHaveTextContent('connecting');
        expect(screen.queryByText('Local owner host')).not.toBeInTheDocument();
        expect(screen.queryByRole('alert')).not.toBeInTheDocument();
        expect(screen.queryByLabelText('SSH destination')).not.toBeInTheDocument();
        await waitFor(() => expect(api.markReady).toHaveBeenCalledOnce());
        await fireEvent.click(remote);
        resolveRemote({...snapshot, owner: {...snapshot.owner, id: 'owner-b'}});
        expect(await screen.findByRole('heading', {name: 'owner-b'})).toBeInTheDocument();
        expect(screen.getByRole('button', {name: 'Show owner owner-b at user@host'})).toHaveTextContent('connected');
        expect(screen.queryByRole('heading', {name: 'sandbox'})).not.toBeInTheDocument();
    });
    it('preserves a connection-store error when the local backend reports remote-only', async () => {
        localStorage.clear();
        const api = mockApi({
            listConnections: vi.fn().mockRejectedValue({code: 'store_unavailable', message: 'Saved connections could not be read.'}),
            loadFleet: vi.fn().mockRejectedValue({code: 'remote_only', message: 'This platform supports remote owner hosts. Add an SSH connection.'})
        });
        render(FleetApp, {api});
        expect(await screen.findByRole('alert')).toHaveTextContent('Saved connections could not be read.');
        expect(screen.getByLabelText('SSH destination')).toBeInTheDocument();
    });
    it('previews explicit yard power and reconciliation commands in the selected owner context', async () => {
        localStorage.clear();
        const api = mockApi({loadYard: vi.fn().mockResolvedValue({profiles: [], settings: [], diagnostics: [],
            capabilities: ['operation-exact-plan-v1','operation-steps-v1']})});
        render(FleetApp, {api});
        await screen.findByRole('heading', {name: 'default'});
        await fireEvent.click(await screen.findByRole('button', {name: 'Stop yard'}));
        await waitFor(() => expect(api.planOperation).toHaveBeenLastCalledWith({connectionId: null, yard: 'default', command: 'stop', arguments: []}));
        expect(screen.queryByRole('button', {name: 'Start yard'})).not.toBeInTheDocument();
        await fireEvent.click(screen.getByRole('button', {name: 'Show yard sandbox'}));
        await fireEvent.click(await screen.findByRole('button', {name: 'Start yard'}));
        await waitFor(() => expect(api.planOperation).toHaveBeenLastCalledWith({connectionId: null, yard: 'sandbox', command: 'start', arguments: []}));
        await fireEvent.click(screen.getByRole('button', {name: 'Reconcile yard'}));
        await waitFor(() => expect(api.planOperation).toHaveBeenLastCalledWith({connectionId: null, yard: 'sandbox', command: 'init', arguments: []}));
        expect(api.executeOperation).not.toHaveBeenCalled();
    });
    it('releases fingerprint assessments when dismissed or replaced by navigation', async () => {
        localStorage.clear();
        const api = mockApi();
        render(FleetApp, {api});
        await screen.findByRole('heading', {name: 'default'});
        await fireEvent.click(screen.getByRole('button', {name: /Connect remote host/}));
        await fireEvent.input(screen.getByLabelText('SSH destination'), {target: {value: 'user@host'}});
        await fireEvent.click(screen.getByRole('button', {name: 'Test connection'}));
        await screen.findByText('SHA256:verified');
        await fireEvent.click(screen.getByRole('button', {name: 'Cancel connection'}));
        await waitFor(() => expect(api.cancelAssessment).toHaveBeenCalledWith({assessmentId: 'assessment'}));
        await fireEvent.click(screen.getByRole('button', {name: /Connect remote host/}));
        await fireEvent.click(screen.getByRole('button', {name: 'Test connection'}));
        await screen.findByText('SHA256:verified');
        await fireEvent.click(screen.getByRole('button', {name: 'Show owner owner-a'}));
        await waitFor(() => expect(api.cancelAssessment).toHaveBeenCalledTimes(2));
        expect(api.connectRemote).not.toHaveBeenCalled();
    });
    it('previews new-yard creation only from a reported yard preset on the owner', async () => {
        localStorage.clear();
        const api = mockApi({loadHost: vi.fn().mockResolvedValue({settings: [], diagnostics: [], sync: null,
            capabilities: ['profile-list-v1','yard-bootstrap-v1','operation-exact-plan-v1','operation-steps-v1'],
            profiles: [{name: 'development', hasYardPreset: true}, {name: 'tools', hasYardPreset: false}]
        })});
        render(FleetApp, {api});
        await screen.findByRole('heading', {name: 'default'});
        await fireEvent.click(screen.getByRole('button', {name: 'Show owner owner-a'}));
        await fireEvent.click(screen.getByRole('button', {name: 'Yards'}));
        await fireEvent.click(await screen.findByRole('button', {name: 'Create yard'}));
        expect(screen.getByLabelText('Yard preset')).toHaveTextContent('development');
        expect(screen.getByLabelText('Yard preset')).not.toHaveTextContent('tools');
        await fireEvent.input(screen.getByLabelText('Yard name'), {target: {value: 'default'}});
        expect(screen.getByRole('button', {name: 'Preview creation'})).toBeDisabled();
        await fireEvent.input(screen.getByLabelText('Yard name'), {target: {value: 'new-yard'}});
        await fireEvent.click(screen.getByRole('button', {name: 'Preview creation'}));
        await waitFor(() => expect(api.planOperation).toHaveBeenCalledWith({connectionId: null, yard: 'new-yard',
            command: 'init', arguments: ['--profile', 'development']}));
    });
    it('preserves multiline setting values in the editor and exact plan arguments', async () => {
        localStorage.clear();
        const original = 'first line\nsecond line';
        const updated = 'first line\nsecond line\nthird line';
        const api = mockApi({loadYard: vi.fn().mockResolvedValue({
            profiles: [], diagnostics: [],
            capabilities: ['settings-list-v1', 'operation-exact-plan-v1', 'operation-steps-v1'],
            settings: [{name: 'EXTRA_CONFIGURATION', kind: 'scalar', type: 'multiline', value: original,
                        valueAvailable: true, editable: true, provenance: []}]
        })});
        render(FleetApp, {api});
        await screen.findByRole('heading', {name: 'default'});
        await fireEvent.click(screen.getByRole('button', {name: 'Settings'}));
        await fireEvent.click(await screen.findByRole('button', {name: 'Edit'}));
        const editor = screen.getByLabelText('EXTRA_CONFIGURATION');
        expect(editor.tagName).toBe('TEXTAREA');
        expect(editor).toHaveValue(original);
        await fireEvent.input(editor, {target: {value: updated}});
        await fireEvent.click(screen.getByRole('button', {name: 'Preview change'}));
        await waitFor(() => expect(api.planOperation).toHaveBeenCalledWith({connectionId: null, yard: 'default',
            command: 'config', arguments: ['set', 'EXTRA_CONFIGURATION', updated, '--scope', 'yard']}));
    });
    it('shows the usable fleet and marks ready while yard details are still pending', async () => {
        localStorage.clear();
        let resolveDetails: (value: Awaited<ReturnType<VerandaApi['loadYard']>>) => void = () => {};
        const api = mockApi({
            loadYard: vi.fn().mockImplementation(() => new Promise(resolve => { resolveDetails = resolve; })),
            markReady: vi.fn().mockResolvedValue(undefined)
        });
        render(FleetApp, { api });
        expect(await screen.findByRole('heading', { name: 'default' })).toBeInTheDocument();
        expect(screen.getByRole('list', { name: 'Projects in default' })).toHaveTextContent('Alpha');
        expect(screen.getByRole('status')).toHaveTextContent('Loading yard details');
        await waitFor(() => expect(api.markReady).toHaveBeenCalledOnce());
        await fireEvent.click(screen.getByRole('button', { name: 'Profiles' }));
        expect(screen.queryByText('Profiles unavailable. Requires profile-list-v1.')).not.toBeInTheDocument();
        resolveDetails({profiles: [], settings: [], diagnostics: [], capabilities: []});
        expect(await screen.findByText('Profiles unavailable. Requires profile-list-v1.')).toBeInTheDocument();
        expect(screen.queryByText('Loading yard details…')).not.toBeInTheDocument();
    });
    it('requires explicit fingerprint acceptance before opening a trusted connection', async () => {
        localStorage.clear();
        const api = mockApi();
        render(FleetApp, { api });
        await screen.findByRole('heading', { name: 'default' });
        await fireEvent.click(screen.getByRole('button', { name: /Connect remote host/ }));
        await fireEvent.input(screen.getByLabelText('SSH destination'), { target: { value: 'user@host' } });
        await fireEvent.click(screen.getByRole('button', { name: 'Test connection' }));
        expect(await screen.findByText('SHA256:verified')).toBeInTheDocument();
        expect(screen.getByRole('button', { name: 'Connect and save' })).toBeDisabled();
        expect(api.connectRemote).not.toHaveBeenCalled();
        await fireEvent.click(screen.getByRole('checkbox', { name: 'I verified and accept this host key' }));
        await fireEvent.click(screen.getByRole('button', { name: 'Connect and save' }));
        await waitFor(() => expect(api.connectRemote).toHaveBeenCalledWith({ assessmentId: 'assessment', fingerprint: 'SHA256:verified', confirmed: true }));
    });
    it('shows matched-release guidance on compatibility errors', async () => {
        localStorage.clear();
        render(FleetApp, { api: mockApi({ loadFleet: vi.fn().mockRejectedValue({ code: 'incompatible', message: 'Incompatible Subyard version 0.2.0; Veranda 0.1.0.' }) }) });
        expect(await screen.findByRole('alert')).toHaveTextContent('Install Veranda and Subyard from the same release');
    });
    it('does not label a selected profile Applied until owner convergence is current', async () => {
        localStorage.clear();
        const api = mockApi({ loadYard: vi.fn().mockResolvedValue({ profiles: [{ name: 'tools', selected: true, eligible: true, provisionable: true, provisionScope: 'yard', eligibility: 'allowed', convergence: 'unknown', resources: [] }], settings: [], diagnostics: [], capabilities: ['profile-list-v1', 'operation-exact-plan-v1', 'operation-steps-v1'] }) });
        render(FleetApp, { api });
        await screen.findByRole('heading', { name: 'default' });
        await fireEvent.click(screen.getByRole('button', { name: 'Profiles' }));
        expect(screen.getByRole('checkbox', { name: 'tools' })).toBeChecked();
        expect(screen.getByText('Unavailable')).toBeInTheDocument();
        expect(screen.queryByText('Applied')).not.toBeInTheDocument();
    });
    it('reviews structured effects and requires exact-plan consent before execution', async () => {
        localStorage.clear();
        const api = mockApi({
            loadYard: vi.fn().mockResolvedValue({ profiles: [{ name: 'tools', selected: true, eligible: true, provisionable: true, provisionScope: 'yard', eligibility: 'allowed', convergence: 'unknown', resources: [] }], settings: [], diagnostics: [], capabilities: ['profile-list-v1', 'operation-exact-plan-v1', 'operation-steps-v1'] }),
            planOperation: vi.fn().mockResolvedValue({ planId: 'plan', digest: 'digest', operationId: 'operation', summary: 'Update selection', confirmation: 'prompt-default-yes', consequences: ['Selection changes only.'], expiresAt: '2099-10-05T18:00:00Z', steps: [{ id: 'selection', target: 'Profiles', observed: 'tools', desired: 'empty', decision: 'apply', preconditions: ['Current revision'], dependsOn: [], verify: 'Read selection' }] }),
            executeOperation: vi.fn().mockResolvedValue({ operationId: 'operation' })
        });
        render(FleetApp, { api });
        await screen.findByRole('heading', { name: 'default' });
        await fireEvent.click(screen.getByRole('button', { name: 'Profiles' }));
        await fireEvent.click(screen.getByRole('checkbox', { name: 'tools' }));
        expect(await screen.findByText('Read selection')).toBeInTheDocument();
        expect(api.planOperation).toHaveBeenCalledWith({ connectionId: null, yard: 'default', command: 'config', arguments: ['set', 'ENVIRONMENT_PROFILES', '', '--scope', 'yard'] });
        expect(screen.getByRole('button', { name: 'Apply plan' })).toBeDisabled();
        expect(api.executeOperation).not.toHaveBeenCalled();
        await fireEvent.click(screen.getByRole('checkbox', { name: 'I confirm this exact plan' }));
        await fireEvent.click(screen.getByRole('button', { name: 'Apply plan' }));
        await waitFor(() => expect(api.executeOperation).toHaveBeenCalledWith({ planId: 'plan', digest: 'digest', confirmed: true }));
    });
});
it('preserves owner selection when refreshing instead of switching to the current yard', async () => {
    const loader = vi.fn().mockResolvedValue(snapshot);
    render(FleetApp, { loader });
    await screen.findByRole('heading', { name: 'default' });
    await fireEvent.click(screen.getByRole('button', { name: 'Show owner owner-a' }));
    await fireEvent.click(screen.getByRole('button', { name: 'Refresh local fleet' }));
    await waitFor(() => expect(loader).toHaveBeenCalledTimes(2));
    expect(screen.getByRole('heading', { name: 'owner-a' })).toBeInTheDocument();
});

it('removes a saved connection only through its assessed token and explicit consent', async () => {
    localStorage.clear();
    const connection = {id: 'remote', hostId: 'owner-b', destination: 'user@host', authLabel: 'SSH agent'};
    const api = mockApi({
        listConnections: vi.fn().mockResolvedValue([connection]),
        loadFleet: vi.fn().mockImplementation(({connectionId}) => Promise.resolve(connectionId ? {...snapshot, owner: {...snapshot.owner, id: 'owner-b'}} : snapshot)),
        assessRemoval: vi.fn().mockResolvedValue({assessmentId: 'remove-token', confirmation: 'prompt-default-yes', consequences: ['Forget the pinned key.'], connection}),
        removeConnection: vi.fn().mockResolvedValue(undefined)
    });
    render(FleetApp, {api});
    await screen.findByRole('heading', {name: 'default'});
    await fireEvent.click(screen.getByRole('button', {name: 'Show owner owner-b at user@host'}));
    await fireEvent.click(screen.getByRole('button', {name: 'Remove connection'}));
    expect(await screen.findByText('Forget the pinned key.')).toBeInTheDocument();
    expect(api.removeConnection).not.toHaveBeenCalled();
    await fireEvent.click(screen.getByRole('button', {name: 'Confirm removal'}));
    await waitFor(() => expect(api.removeConnection).toHaveBeenCalledWith({assessmentId: 'remove-token', confirmed: true}));
});

it('renders owner selection provenance and numeric credential summaries', async () => {
    localStorage.clear();
    const api = mockApi({
        loadYard: vi.fn().mockResolvedValue({profiles: [], settings: [], diagnostics: [], selection: {value: 'sample', provenance: [{scope: 'yard', role: 'local', status: 'effective'}]}, capabilities: ['profile-list-v1']}),
        loadHost: vi.fn().mockResolvedValue({settings: [], diagnostics: [], capabilities: ['host-sync-status-v1'], sync: {schemaVersion: 1, hostId: 'owner-a', hostIdPending: false, automation: 'manual', offline: true, registration: 'not-configured', recoveryRequired: false, generation: 0, appliedCommit: '', credentials: {state: 'available', records: 3, conflicts: 1, peers: [{name: 'peer-a', role: 'owner', trusted: true, manualOnly: true, lastAttempt: 0, lastSuccess: 0, consecutiveFailures: 0, nextRetry: 0, failed: false}]}}})
    });
    render(FleetApp, {api}); await screen.findByRole('heading', {name: 'default'});
    await fireEvent.click(screen.getByRole('button', {name: 'Profiles'}));
    expect(screen.getByText('Effective selection: sample')).toBeInTheDocument();
    expect(screen.getByRole('list', {name: 'Profile selection provenance'})).toHaveTextContent('yard · local · effective');
    await fireEvent.click(screen.getByRole('button', {name: 'Show owner owner-a'}));
    await fireEvent.click(screen.getByRole('button', {name: 'Sync'}));
    await waitFor(() => expect(screen.getByText('Records: 3 · Conflicts: 1')).toBeInTheDocument());
    expect(screen.getByRole('list', {name: 'Credential peers'})).toHaveTextContent('peer-a');
});

it('updates a host from native snapshots and disables stale mutations until fresh data arrives', async () => {
    localStorage.clear();
    let listener: (event: import('./types').VerandaEvent) => void = () => {};
    const details = {profiles: [{name: 'tools', selected: true, eligible: true, provisionable: true, provisionScope: 'yard', eligibility: 'allowed', convergence: 'unknown', resources: []}], settings: [], diagnostics: [], capabilities: ['profile-list-v1','operation-exact-plan-v1','operation-steps-v1']};
    const api = mockApi({loadYard: vi.fn().mockResolvedValue(details), listen: vi.fn().mockImplementation(handler => {listener = handler; return Promise.resolve(() => {});})});
    render(FleetApp, {api}); await screen.findByRole('heading', {name: 'default'});
    await fireEvent.click(screen.getByRole('button', {name: 'Profiles'}));
    for (const state of ['stale', 'disconnected', 'reconnecting']) {
        listener({connectionId: null, yard: 'sandbox', state});
        await tick();
        expect(screen.getByRole('checkbox', {name: 'tools'})).toBeEnabled();
    }
    listener({connectionId: null, yard: 'default', state: 'stale'});
    await waitFor(() => expect(screen.getByRole('checkbox', {name: 'tools'})).toBeDisabled());
    const updated = {...snapshot, owner: {...snapshot.owner, yards: snapshot.owner.yards.map(y => y.name === 'default' ? {...y, projects: [{id: 'fresh', name: 'Fresh project'}]} : y)}};
    listener({connectionId: null, yard: 'sandbox', state: 'connected', snapshot: updated});
    await tick();
    expect(screen.getByRole('checkbox', {name: 'tools'})).toBeDisabled();
    listener({connectionId: null, yard: 'default', state: 'connected', snapshot: updated});
    await waitFor(() => expect(screen.getByRole('checkbox', {name: 'tools'})).not.toBeDisabled());
    await fireEvent.click(screen.getByRole('button', {name: 'Projects'}));
    expect(screen.getByRole('list', {name: 'Projects in default'})).toHaveTextContent('Fresh project');
    expect(api.loadFleet).toHaveBeenCalledTimes(1);
});

it('keeps named-yard health independent across selection and late detail responses', async () => {
    localStorage.clear();
    let listener: (event: VerandaEvent) => void = () => {};
    const details = {profiles: [{name: 'tools', selected: true, eligible: true, provisionable: true, provisionScope: 'yard', eligibility: 'allowed', convergence: 'unknown', resources: []}], settings: [], diagnostics: [], capabilities: ['profile-list-v1','operation-exact-plan-v1','operation-steps-v1']};
    let resolveOld: (value: typeof details) => void = () => {};
    const api = mockApi({
        listen: vi.fn().mockImplementation(handler => { listener = handler; return Promise.resolve(() => {}); }),
        loadYard: vi.fn().mockResolvedValueOnce(details).mockResolvedValueOnce(details)
            .mockImplementationOnce(() => new Promise(resolve => { resolveOld = resolve; }))
            .mockResolvedValue(details)
    });
    render(FleetApp, {api});
    await screen.findByRole('heading', {name: 'default'});
    await fireEvent.click(screen.getByRole('button', {name: 'Profiles'}));
    listener({connectionId: null, yard: 'default', state: 'stale'});
    await waitFor(() => expect(screen.getByRole('checkbox', {name: 'tools'})).toBeDisabled());
    await fireEvent.click(screen.getByRole('button', {name: 'Show yard sandbox'}));
    await fireEvent.click(screen.getByRole('button', {name: 'Profiles'}));
    await waitFor(() => expect(screen.getByRole('checkbox', {name: 'tools'})).toBeEnabled());
    await fireEvent.click(screen.getByRole('button', {name: 'Show yard default'}));
    await waitFor(() => expect(api.loadYard).toHaveBeenCalledTimes(3));
    listener({connectionId: null, yard: 'default', state: 'disconnected'});
    resolveOld(details);
    await fireEvent.click(screen.getByRole('button', {name: 'Profiles'}));
    await waitFor(() => expect(screen.getByRole('checkbox', {name: 'tools'})).toBeDisabled());
    expect(screen.getByRole('alert')).toHaveTextContent('disconnected');
    listener({connectionId: null, yard: 'default', state: 'connected', snapshot});
    await waitFor(() => expect(screen.getByRole('checkbox', {name: 'tools'})).toBeEnabled());
    listener({connectionId: null, state: 'stale'});
    await fireEvent.click(screen.getByRole('button', {name: 'Show yard sandbox'}));
    await fireEvent.click(screen.getByRole('button', {name: 'Profiles'}));
    await waitFor(() => expect(screen.getByRole('checkbox', {name: 'tools'})).toBeDisabled());
});

it.each(['default', undefined])('preserves newer %s health events during a deferred fleet refresh', async yard => {
    localStorage.clear();
    let listener: (event: VerandaEvent) => void = () => {};
    let resolveFleet: (value: LocalFleetSnapshot) => void = () => {};
    const details = {profiles: [{name: 'tools', selected: true, eligible: true, provisionable: true, provisionScope: 'yard', eligibility: 'allowed', convergence: 'unknown', resources: []}], settings: [], diagnostics: [], capabilities: ['profile-list-v1','operation-exact-plan-v1','operation-steps-v1']};
    const api = mockApi({
        listen: vi.fn().mockImplementation(handler => {listener = handler; return Promise.resolve(() => {});}),
        loadYard: vi.fn().mockResolvedValue(details),
        loadFleet: vi.fn().mockResolvedValueOnce(snapshot).mockImplementationOnce(() => new Promise(resolve => {resolveFleet = resolve;}))
    });
    render(FleetApp, {api});
    await screen.findByRole('heading', {name: 'default'});
    await fireEvent.click(screen.getByRole('button', {name: 'Profiles'}));
    await fireEvent.click(screen.getByRole('button', {name: 'Refresh local fleet'}));
    await waitFor(() => expect(api.loadFleet).toHaveBeenCalledTimes(2));
    listener({connectionId: null, yard, state: 'disconnected'});
    resolveFleet(snapshot);
    await waitFor(() => expect(screen.getByRole('button', {name: 'Refresh local fleet'})).toBeEnabled());
    await waitFor(() => expect(screen.getByRole('checkbox', {name: 'tools'})).toBeDisabled());
    expect(screen.getByRole('alert')).toHaveTextContent('disconnected');
    listener({connectionId: null, yard, state: 'connected', snapshot});
    await waitFor(() => expect(screen.getByRole('checkbox', {name: 'tools'})).toBeEnabled());
});

it('keeps a failed cancellation running and does not treat unknown as completion', async () => {
    localStorage.clear();
    let listener: (event: import('./types').VerandaEvent) => void = () => {};
    const api = mockApi({
        loadYard: vi.fn().mockResolvedValue({profiles: [{name: 'tools', selected: true, eligible: true, provisionable: true, provisionScope: 'yard', eligibility: 'allowed', convergence: 'unknown', resources: []}], settings: [], diagnostics: [], capabilities: ['profile-list-v1','operation-exact-plan-v1','operation-steps-v1']}),
        listen: vi.fn().mockImplementation(handler => {listener = handler; return Promise.resolve(() => {});}),
        planOperation: vi.fn().mockResolvedValue({planId: 'plan', digest: 'digest', operationId: 'operation', summary: 'Selection', confirmation: 'never', consequences: [], expiresAt: '2099-01-01T00:00:00Z', steps: []}),
        executeOperation: vi.fn().mockResolvedValue({operationId: 'operation'}),
        cancelOperation: vi.fn().mockRejectedValue({message: 'Cancellation unavailable.'})
    });
    render(FleetApp, {api}); await screen.findByRole('heading', {name: 'default'});
    await fireEvent.click(screen.getByRole('button', {name: 'Profiles'}));
    await fireEvent.click(screen.getByRole('checkbox', {name: 'tools'}));
    await screen.findByRole('button', {name: 'Apply plan'});
    await fireEvent.click(screen.getByRole('button', {name: 'Apply plan'}));
    await screen.findByRole('button', {name: 'Cancel operation'});
    await fireEvent.click(screen.getByRole('button', {name: 'Cancel operation'}));
    expect(await screen.findByRole('alert')).toHaveTextContent('Cancellation unavailable');
    expect(screen.getByRole('status')).toHaveTextContent('running');
    listener({connectionId: null, operationId: 'operation', state: 'unknown'});
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('unknown'));
    expect(api.executeOperation).toHaveBeenCalledTimes(1);
    expect(api.loadFleet).toHaveBeenCalledTimes(1);
});

it('retains the most recent 200 operation messages through a burst without querying the fleet again', async () => {
    localStorage.clear();
    let listener: (event: VerandaEvent) => void = () => {};
    const api = mockApi({
        listen: vi.fn().mockImplementation(callback => { listener = callback; return Promise.resolve(() => {}); }),
        loadYard: vi.fn().mockResolvedValue({ profiles: [{ name: 'tools', selected: true, eligible: true, provisionable: true, provisionScope: 'yard', eligibility: 'allowed', convergence: 'current', resources: [] }], settings: [], diagnostics: [], capabilities: ['profile-list-v1', 'operation-exact-plan-v1', 'operation-steps-v1'] }),
        planOperation: vi.fn().mockResolvedValue({planId: 'plan', digest: 'digest', operationId: 'history', summary: 'Selection', confirmation: 'never', consequences: [], expiresAt: '2099-01-01T00:00:00Z', steps: []}),
        executeOperation: vi.fn().mockResolvedValue({operationId: 'history'})
    });
    render(FleetApp, {api});
    await screen.findByRole('heading', {name: 'default'});
    await fireEvent.click(screen.getByRole('button', {name: 'Profiles'}));
    await fireEvent.click(screen.getByRole('checkbox', {name: 'tools'}));
    await fireEvent.click(await screen.findByRole('button', {name: 'Apply plan'}));
    await screen.findByRole('button', {name: 'Cancel operation'});
    const detailCalls = vi.mocked(api.loadYard).mock.calls.length;
    for (let index = 0; index < 1000; index++)
        listener({connectionId: null, operationId: 'history', state: 'running', message: `Event ${index}`});
    await tick();
    const operation = within(screen.getByRole('region', {name: 'Operation'}));
    expect(operation.getAllByRole('listitem').map(row => row.textContent?.trim()))
        .toEqual(Array.from({length: 200}, (_, index) => `Event ${index + 800}`));
    expect(operation.queryByText('Event 0', {exact: true})).not.toBeInTheDocument();
    expect(operation.getByText('Event 999', {exact: true})).toBeInTheDocument();
    expect(operation.getByRole('status')).toHaveTextContent('running');
    expect(api.loadFleet).toHaveBeenCalledTimes(1);
    expect(api.loadYard).toHaveBeenCalledTimes(detailCalls);
    expect(api.executeOperation).toHaveBeenCalledTimes(1);
});

it('coalesces connected snapshot bursts into one detail query while displaying the latest projects', async () => {
    localStorage.clear();
    let listener: (event: VerandaEvent) => void = () => {};
    const api = mockApi({listen: vi.fn().mockImplementation(callback => { listener = callback; return Promise.resolve(() => {}); })});
    const view = render(FleetApp, {api});
    await screen.findByRole('heading', {name: 'default'});
    await waitFor(() => expect(api.loadYard).toHaveBeenCalledTimes(1));
    vi.useFakeTimers();
    try {
        for (let index = 0; index < 1000; index++)
            listener({connectionId: null, yard: 'default', state: 'connected', snapshot: {...snapshot, owner: {...snapshot.owner,
                yards: snapshot.owner.yards.map(yard => yard.name === 'default' ? {...yard, projects: [{id: 'alpha', name: `Project ${index}`}]} : yard)}}});
        await tick();
        expect(screen.getByRole('list', {name: 'Projects in default'})).toHaveTextContent('Project 999');
        expect(screen.queryByText('Alpha', {exact: true})).not.toBeInTheDocument();
        expect(api.loadFleet).toHaveBeenCalledTimes(1);
        expect(api.loadYard).toHaveBeenCalledTimes(1);
        await vi.advanceTimersByTimeAsync(100);
        listener({connectionId: null, yard: 'sandbox', state: 'connected', snapshot: {...snapshot, owner: {...snapshot.owner,
            yards: snapshot.owner.yards.map(yard => ({...yard, projects: yard.name === 'default'
                ? [{id: 'alpha', name: 'Project 999'}] : [{id: 'background', name: 'Background project'}]}))}}});
        await vi.advanceTimersByTimeAsync(149);
        expect(api.loadYard).toHaveBeenCalledTimes(1);
        await vi.advanceTimersByTimeAsync(1);
        expect(api.loadYard).toHaveBeenCalledTimes(2);
        expect(api.loadYard).toHaveBeenLastCalledWith({connectionId: null, yard: 'default'});
        await vi.advanceTimersByTimeAsync(1000);
        expect(api.loadYard).toHaveBeenCalledTimes(2);
        expect(api.loadFleet).toHaveBeenCalledTimes(1);
        await fireEvent.click(screen.getByRole('button', {name: 'Show yard sandbox'}));
        expect(screen.getByRole('list', {name: 'Projects in sandbox'})).toHaveTextContent('Background project');
    } finally {
        view.unmount();
        vi.useRealTimers();
    }
});

it('disposes a late subscription once and cancels queued details when unmounted', async () => {
    localStorage.clear();
    let listener: (event: VerandaEvent) => void = () => {};
    let resolveListen: (dispose: () => void) => void = () => {};
    const dispose = vi.fn();
    const api = mockApi({listen: vi.fn().mockImplementation(callback => {
        listener = callback;
        return new Promise<() => void>(resolve => { resolveListen = resolve; });
    })});
    const view = render(FleetApp, {api});
    let mounted = true;
    await screen.findByRole('heading', {name: 'default'});
    await waitFor(() => expect(api.loadYard).toHaveBeenCalledTimes(1));
    vi.useFakeTimers();
    try {
        listener({connectionId: null, state: 'connected', snapshot});
        await tick();
        expect(vi.getTimerCount()).toBe(1);
        view.unmount();
        mounted = false;
        expect(vi.getTimerCount()).toBe(0);
        resolveListen(dispose);
        await tick();
        expect(dispose).toHaveBeenCalledTimes(1);
        listener({connectionId: null, state: 'connected', snapshot: {...snapshot, owner: {...snapshot.owner, id: 'late-owner'}}});
        await tick();
        expect(view.container).toBeEmptyDOMElement();
        expect(vi.getTimerCount()).toBe(0);
        await vi.advanceTimersByTimeAsync(1000);
        expect(api.loadYard).toHaveBeenCalledTimes(1);
        expect(api.loadHost).not.toHaveBeenCalled();
        expect(api.loadFleet).toHaveBeenCalledTimes(1);
        expect(dispose).toHaveBeenCalledTimes(1);
    } finally {
        if (mounted) view.unmount();
        vi.useRealTimers();
    }
});

it('disables every session action when the owner lacks prepared sessions', async () => {
    localStorage.clear();
    const api = mockApi({loadFleet: vi.fn().mockResolvedValue({...snapshot, capabilities: []})});
    render(FleetApp, {api});
    await screen.findByRole('heading', {name: 'default'});
    for (const button of screen.getAllByRole('button', {name: /^(Shell|Open in VS Code|Copy shell command)/})) expect(button).toBeDisabled();
    await fireEvent.click(screen.getByRole('button', {name: 'Show owner owner-a'}));
    expect(screen.getByRole('button', {name: 'CPU / RAM terminal'})).toBeDisabled();
    expect(api.launchSession).not.toHaveBeenCalled();
    expect(api.shellCommand).not.toHaveBeenCalled();
});
it.each(['capability_missing', 'invalid_response'])('explains typed compatibility failure %s without inventing versions', async code => {
    localStorage.clear();
    const api = mockApi({loadFleet: vi.fn().mockRejectedValue({code, message: 'Owner response unavailable.'})});
    render(FleetApp, {api});
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Install Veranda and Subyard from the same release');
    expect(alert).not.toHaveTextContent('Veranda: Unavailable; Subyard: Unavailable');
});
it('preserves the native mismatch versions without contradictory unknown versions', async () => {
    localStorage.clear();
    const message = 'Veranda 0.1.0 and Subyard 0.1.1 are incompatible. Install both from the same release.';
    const api = mockApi({loadFleet: vi.fn().mockRejectedValue({code: 'incompatible_engine', message})});
    render(FleetApp, {api});
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(message);
    expect(alert).not.toHaveTextContent('Unavailable');
});
it('labels cached versions honestly when the owner rejects a profile schema', async () => {
    localStorage.clear();
    const api = mockApi({
        loadFleet: vi.fn().mockResolvedValue({...snapshot, verandaVersion: '0.4.0'}),
        loadYard: vi.fn().mockRejectedValue({code: 'incompatible_engine', message: 'The owner profile catalog is incompatible. Install Veranda and Subyard from the same release.'})
    });
    render(FleetApp, {api});
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Last known versions: Veranda: 0.4.0; Subyard: 0.4.0');
});
it('keeps sync actions unavailable and unknown counts honest without typed status', async () => {
    localStorage.clear();
    const api = mockApi({loadHost: vi.fn().mockResolvedValue({settings: [], diagnostics: [], sync: null, capabilities: ['operation-exact-plan-v1','operation-steps-v1']})});
    render(FleetApp, {api});
    await screen.findByRole('heading', {name: 'default'});
    await fireEvent.click(screen.getByRole('button', {name: 'Show owner owner-a'}));
    await fireEvent.click(screen.getByRole('button', {name: 'Sync'}));
    await fireEvent.input(screen.getByLabelText('Configuration repository URL'), {target: {value: 'https://example.com/config.git'}});
    expect(screen.getByRole('button', {name: 'Preview repository connection'})).toBeDisabled();
    expect(screen.getByRole('button', {name: 'Sync now'})).toBeDisabled();
    expect(screen.getByText(/Records: Unavailable/)).toHaveTextContent('Conflicts: Unavailable');
});
it('disables an open settings editor when its owner becomes stale', async () => {
    localStorage.clear();
    let listener: (event: import('./types').VerandaEvent) => void = () => {};
    const api = mockApi({
        listen: vi.fn().mockImplementation(handler => {listener = handler; return Promise.resolve(() => {});}),
        loadYard: vi.fn().mockResolvedValue({profiles: [], diagnostics: [], capabilities: ['settings-list-v1','operation-exact-plan-v1','operation-steps-v1'], settings: [{name: 'SETTING', type: 'string', value: 'value', valueAvailable: true, editable: true, provenance: []}]})
    });
    render(FleetApp, {api});
    await screen.findByRole('heading', {name: 'default'});
    await fireEvent.click(screen.getByRole('button', {name: 'Settings'}));
    await fireEvent.click(await screen.findByRole('button', {name: 'Edit'}));
    listener({connectionId: null, state: 'stale'});
    await waitFor(() => expect(screen.getByRole('button', {name: 'Preview change'})).toBeDisabled());
    expect(screen.getByRole('button', {name: 'Preview unset'})).toBeDisabled();
    expect(api.planOperation).not.toHaveBeenCalled();
});
it('ignores an old detail failure after the selected owner reports incompatibility', async () => {
    localStorage.clear();
    let rejectOld: (error: unknown) => void = () => {};
    const api = mockApi({
        loadYard: vi.fn().mockImplementation(() => new Promise((_resolve, reject) => {rejectOld = reject;})),
        loadHost: vi.fn().mockRejectedValue({code: 'capability_missing', message: 'Selected owner cannot provide details.'})
    });
    render(FleetApp, {api});
    await screen.findByRole('heading', {name: 'default'});
    await fireEvent.click(screen.getByRole('button', {name: 'Show owner owner-a'}));
    expect(await screen.findByRole('alert')).toHaveTextContent('Selected owner cannot provide details.');
    rejectOld({code: 'disconnected', message: 'Previous yard closed.'});
    await new Promise(resolve => setTimeout(resolve, 0));
    const alert = screen.getByRole('alert');
    expect(alert).toHaveTextContent('Selected owner cannot provide details.');
    expect(alert).toHaveTextContent('Install Veranda and Subyard from the same release');
    expect(alert).not.toHaveTextContent('Previous yard closed.');
});
