<script lang="ts">
import { onMount } from 'svelte';
import './app.css';
import { nativeApi } from './native';
import type { FleetLoader, LocalFleetSnapshot, VerandaApi, ConnectionRecord, ConnectionAssessment, YardDetails, HostDetails, OperationPlan, Setting, RemovalAssessment } from './types';
export let loader: FleetLoader | undefined = undefined;
export let api: VerandaApi = nativeApi;
type Host = {
    id: string | null;
    snapshot: LocalFleetSnapshot | null;
    record?: ConnectionRecord;
    error?: string;
    errorCode?: string;
    state?: string;
    yardStates?: Record<string, string>;
    ownerStateEpoch?: number;
};
let hosts: Host[] = [], connectionId: string | null = null, yardName: string | null = null;
let tab = 'Projects', loading = true, refreshing = false, detailsLoading = false, busy = false, error = '', errorCode = '';
let details: Partial<YardDetails & HostDetails> = {};
let connecting = false, destination = '', assessment: ConnectionAssessment | null = null, accepted = false, assessmentGeneration = 0;
let planTarget = '';
let repositoryUrl = '';
let plan: OperationPlan | null = null, operationId = '', operationState = '', messages: string[] = [];
let setting: Setting | null = null, settingValue = '', newYard = '', newYardProfile = '', creatingYard = false, removal: RemovalAssessment | null = null, sessionCommand = '', generation = 0, disposed = false;
let fleetGeneration = 0, selectionGeneration = 0;
let copyHint = '';
$: host = hosts.find(h => h.id === connectionId);
$: snapshot = host?.snapshot;
$: yard = snapshot?.owner.yards.find(y => y.name === yardName);
$: tabs = yard ? ['Overview', 'Projects', 'Profiles', 'Diagnostics', 'Settings'] : ['Overview', 'Yards', 'Sync', 'Diagnostics', 'Settings'];
$: canMutate = details.capabilities?.includes('operation-exact-plan-v1') && details.capabilities?.includes('operation-steps-v1');
$: ownerState = host?.state ?? host?.record?.state ?? '';
$: selectionState = unusable(ownerState) ? ownerState : (yardName && host?.yardStates?.[yardName]) || ownerState;
$: unavailable = !snapshot || unusable(selectionState);
$: canSession = snapshot?.capabilities?.includes('session-prepare-v1') === true;
$: canSettings = canMutate && details.capabilities?.includes('settings-list-v1');
$: canSync = canMutate && details.capabilities?.includes('host-sync-status-v1') && !!details.sync;
$: incompatibleError = ['incompatible_engine', 'capability_missing', 'profile_catalog_invalid', 'invalid_response'].includes(errorCode);
$: yardPresets = details.profiles?.filter(profile => profile.hasYardPreset) ?? [];
$: canCreateYard = canMutate && details.capabilities?.includes('yard-bootstrap-v1') && details.capabilities?.includes('profile-list-v1') && yardPresets.length > 0;
function code(e: unknown) { return typeof e === 'object' && e !== null && 'code' in e ? String(e.code) : ''; }
function unusable(state: string) { return ['stale', 'reconnecting', 'disconnected', 'incompatible'].includes(state); }
function retainedYardStates(item: Host, fleet: LocalFleetSnapshot) {
    const names = new Set(fleet.owner.yards.map(yard => yard.name));
    return Object.fromEntries(Object.entries(item.yardStates ?? {}).filter(([name]) => names.has(name)));
}
function failure(e: unknown) { return typeof e === 'object' && e !== null && 'message' in e ? String(e.message) : 'Request failed. Refresh and try again.'; }
async function guarded(fn: () => Promise<void>) { busy = true; error = ''; errorCode = ''; try {
    await fn();
}
catch (e) {
    error = failure(e); errorCode = code(e);
}
finally {
    busy = false;
} }
function stored(): {
    connectionId: string | null;
    yardName: string | null;
    tab: string;
} | null { try {
    return JSON.parse(localStorage.getItem('veranda-selection') ?? 'null');
}
catch {
    return null;
} }
function remember() { try {
    localStorage.setItem('veranda-selection', JSON.stringify({ connectionId, yardName, tab }));
}
catch { } }
async function refresh() {
    const request = ++fleetGeneration, selectionAtStart = selectionGeneration;
    const previous = loading ? (loader ? null : stored()) : snapshot ? { connectionId, yardName, tab } : null;
    refreshing = !loading;
    error = '';
    let connectionError = '';
    const records = loader ? [] : await api.listConnections().catch(e => { connectionError = failure(e); return []; });
    if (disposed || request !== fleetGeneration) return;
    error = connectionError;
    const saved = hosts;
    hosts = [null, ...records].map(record => {
        const id = record?.id ?? null, cached = saved.find(h => h.id === id)?.snapshot ?? null;
        const previousHost = saved.find(h => h.id === id);
        return {id, record: record ?? undefined, snapshot: cached, state: cached ? 'reconnecting' : 'connecting', yardStates: previousHost?.yardStates, ownerStateEpoch: previousHost?.ownerStateEpoch};
    });
    await new Promise<void>(resolve => {
        let pending = hosts.length;
        hosts.forEach(async seeded => {
            let result: Host;
            try {
                const fleet = await (loader ? loader() : api.loadFleet({connectionId: seeded.id}));
                result = {...seeded, state: 'connected', snapshot: fleet};
            }
            catch (e) { result = {...seeded, state: ['incompatible_engine', 'capability_missing', 'profile_catalog_invalid', 'invalid_response'].includes(code(e)) ? 'incompatible' : 'disconnected', error: failure(e), errorCode: code(e)}; }
            if (disposed || request !== fleetGeneration) { resolve(); return; }
            if (result.id === null && result.errorCode === 'remote_only') {
                hosts = hosts.filter(h => h.id !== null);
                if (!records.length && selectionGeneration === selectionAtStart) connecting = true;
                loading = false;
                if (--pending === 0) refreshing = false;
                resolve(); return;
            }
            // A read that started before a health event cannot heal that event.
            const live = hosts.find(h => h.id === result.id) ?? seeded;
            result = {...result, yardStates: result.snapshot ? retainedYardStates(live, result.snapshot) : live.yardStates,
                state: live.ownerStateEpoch === seeded.ownerStateEpoch ? result.state : live.state,
                ownerStateEpoch: live.ownerStateEpoch};
            hosts = hosts.map(h => h.id === result.id ? result : h);
            const unchanged = selectionGeneration === selectionAtStart;
            const current = result.id === connectionId;
            const preferred = unchanged && previous?.connectionId === result.id;
            if (result.snapshot && (current || preferred || (unchanged && !hosts.find(h => h.id === connectionId)?.snapshot))) {
                const desired = current && !unchanged ? {connectionId, yardName, tab} : preferred ? previous : null;
                connectionId = result.id;
                yardName = desired?.yardName === null ? null : result.snapshot.owner.yards.some(y => y.name === desired?.yardName) ? desired!.yardName : result.snapshot.owner.yards.find(y => y.name === result.snapshot!.currentYardName)?.name ?? result.snapshot.owner.yards[0]?.name ?? null;
                tab = desired?.tab ?? (yardName ? 'Projects' : 'Overview');
                if (!yardName && tab === 'Projects') tab = 'Overview';
                error = result.error ?? connectionError; errorCode = result.errorCode ?? '';
                if (!setting && !plan) void loadDetails();
            } else if (current && result.error) { error = result.error; errorCode = result.errorCode ?? ''; }
            if (result.id === null || result.snapshot) { loading = false; resolve(); }
            if (--pending === 0) { loading = false; refreshing = false; resolve(); }
        });
    });
}
async function loadDetails() { const request = ++generation; details = {}; if (loader || !hosts.find(h => h.id === connectionId)?.snapshot)
    { detailsLoading = false; return; } detailsLoading = true; try {
    const next = yardName ? await api.loadYard({ connectionId, yard: yardName }) : await api.loadHost({ connectionId });
    if (request === generation && !disposed)
        details = next;
}
catch (e) {
    if (request === generation && !disposed) {
        error = failure(e); errorCode = code(e);
    }
} finally { if (request === generation && !disposed) detailsLoading = false; } }
async function releaseAssessments() {
    assessmentGeneration++;
    const tokens = [assessment?.assessmentId, removal?.assessmentId].filter((token): token is string => !!token);
    assessment = null; removal = null; accepted = false;
    await Promise.all(tokens.map(assessmentId => api.cancelAssessment({assessmentId}).catch(e => { if (!disposed) { error = failure(e); errorCode = code(e); } })));
}
async function select(id: string | null, name: string | null) { selectionGeneration++; sessionCommand = ''; copyHint = ''; void releaseAssessments(); connectionId = id; yardName = name; tab = name ? 'Projects' : 'Overview'; connecting = false; setting = null; creatingYard = false; error = hosts.find(h => h.id === id)?.error ?? ''; errorCode = hosts.find(h => h.id === id)?.errorCode ?? ''; remember(); await loadDetails(); }
async function assess(repairId?: string) {
    const released = releaseAssessments(), request = assessmentGeneration; await released;
    if (disposed || request !== assessmentGeneration) return;
    connecting = true;
    await guarded(async () => {
        const result = repairId ? await api.repairConnection({connectionId: repairId}) : await api.assessConnection({destination: destination.trim()});
        if (!disposed && request === assessmentGeneration) assessment = result;
        else await api.cancelAssessment({assessmentId: result.assessmentId});
    });
}
async function assessRemoval(id: string) {
    const released = releaseAssessments(), request = assessmentGeneration; await released;
    if (disposed || request !== assessmentGeneration) return;
    await guarded(async () => {
        const result = await api.assessRemoval({connectionId: id});
        if (!disposed && request === assessmentGeneration) removal = result;
        else await api.cancelAssessment({assessmentId: result.assessmentId});
    });
}
async function connect() { if (!assessment || !accepted)
    return; await guarded(async () => { const result = await api.connectRemote({ assessmentId: assessment!.assessmentId, fingerprint: assessment!.fingerprint, confirmed: true }); assessment = null; await refresh(); await select(result.id, null); }); }
async function prepare(command: string, args: string[], targetYard = yardName) { if (unavailable || !canMutate || (args[0] === 'sync' && (command === 'config' || command === 'keys') && !canSync))
    return; const owner = snapshot!.owner.id; selectionGeneration++; await guarded(async () => { plan = await api.planOperation({ connectionId, yard: targetYard, command, arguments: args }); planTarget = owner + (targetYard ? ' / ' + targetYard : ''); accepted = false; }); }
async function execute() { if (!plan || (plan.confirmation !== 'never' && !accepted))
    return; operationId = plan.operationId; operationState = 'running'; messages = []; await guarded(async () => { const result = await api.executeOperation({ planId: plan!.planId, digest: plan!.digest, confirmed: plan!.confirmation === 'never' || accepted }); operationId = result.operationId; plan = null; setting = null; }); }
async function discard() { if (!plan)
    return; await guarded(async () => { await api.discardOperation({ planId: plan!.planId }); plan = null; }); }
async function launch(kind: 'shell' | 'vscode' | 'resources', projectId: string | null = null) { if (unavailable || !canSession || loader) return; const selected = ++selectionGeneration; sessionCommand = ''; copyHint = ''; await guarded(async () => { const result = await api.launchSession({ connectionId, yard: yardName, projectId, kind }); if (!disposed && selected === selectionGeneration) {sessionCommand = result.command ?? ''; copyHint = '';} }); }
async function copyShell(projectId: string | null = null) {
    if (unavailable || !canSession || loader) return;
    const selected = ++selectionGeneration, id = connectionId, name = yardName;
    sessionCommand = ''; copyHint = '';
    await guarded(async () => {
        const result = await api.shellCommand({connectionId: id, yard: name, projectId});
        if (disposed || selected !== selectionGeneration || connectionId !== id || yardName !== name) return;
        sessionCommand = result.command;
        copyHint = 'Select the command below and copy it manually.';
        if (navigator.clipboard?.writeText) {
            try { await navigator.clipboard.writeText(result.command); if (selected === selectionGeneration) copyHint = 'Shell command copied.'; }
            catch { /* A selectable command remains available when clipboard access is denied. */ }
        }
    });
}
function timestamp(seconds: number) { const date = new Date(seconds * 1000); return !seconds || Number.isNaN(date.getTime()) ? 'Unavailable' : date.toISOString(); }
function value(v: unknown) { return v === null || v === undefined ? 'Unavailable' : String(v); }
onMount(() => {
    void refresh().finally(() => { if (!loader && api.markReady) requestAnimationFrame(() => requestAnimationFrame(() => {
        if (disposed) return;
        const snapshots = hosts.flatMap(item => item.snapshot ? [item.snapshot] : []);
        const yards = snapshots.flatMap(item => item.owner.yards);
        void api.markReady!({owners: snapshots.length, yards: yards.length, projects: yards.reduce((sum, item) => sum + item.projects.length, 0)}).catch(() => {});
    })); });
    let stop: (() => void) | undefined;
    let detailTimer: ReturnType<typeof setTimeout> | undefined;
    if (!loader)
        void api.listen(event => {
            if (disposed) return;
            const selectedScope = event.connectionId === connectionId && (!event.yard || event.yard === yardName);
            const ownerScope = !event.yard || selectedScope;
            if (event.snapshot) {
                hosts = hosts.map(h => h.id === event.connectionId ? {...h, snapshot: event.snapshot!, yardStates: retainedYardStates(h, event.snapshot!), error: ownerScope ? undefined : h.error, state: !event.yard ? event.state : h.state} : h);
                if (selectedScope && !setting && !plan) {
                    if (detailTimer) clearTimeout(detailTimer);
                    const selectedConnection = connectionId, selectedYard = yardName;
                    detailTimer = setTimeout(() => {
                        if (!disposed && !setting && !plan && connectionId === selectedConnection && yardName === selectedYard && (!event.yard || event.yard === yardName)) void loadDetails();
                    }, 250);
                }
            }
            if (event.operationId === operationId) {
                operationState = event.state;
                if (event.message)
                    messages = [...messages.slice(-199), event.message];
                if (['completed', 'failed', 'cancelled'].includes(event.state))
                    void refresh();
            }
            if (selectedScope && operationId && ['disconnected', 'stale', 'incompatible'].includes(event.state) && !['completed', 'failed', 'cancelled'].includes(operationState)) operationState = event.state + '; final result unavailable';
            if (['connected', 'reconnecting', 'stale', 'disconnected', 'incompatible'].includes(event.state))
                hosts = hosts.map(h => {
                    if (h.id !== event.connectionId) return h;
                    if (!event.yard) return {...h, state: event.state, ownerStateEpoch: (h.ownerStateEpoch ?? 0) + 1};
                    return h.snapshot?.owner.yards.some(yard => yard.name === event.yard)
                        ? {...h, yardStates: {...h.yardStates, [event.yard]: event.state}} : h;
                });
        }).then(fn => { if (disposed)
            fn();
        else
            stop = fn; }).catch(e => { error = failure(e); errorCode = code(e); });
    return () => { disposed = true; generation++; void releaseAssessments(); if (detailTimer) clearTimeout(detailTimer); stop?.(); };
});
</script>
<a class="skip-link" href="#main-content">Skip to content</a>
<div class="app-shell">
<header class="topbar">
<div class="brand-mark" aria-hidden="true">S</div>
<div>
<p class="product">Subyard</p>
<p class="app-name">Veranda</p>
</div>
<p class="tagline">Nice view into every yard.</p>
<button class="refresh" disabled={loading || refreshing} on:click={refresh}>
<span aria-hidden="true">↻</span> Refresh local fleet</button>
</header>
{#if loading}
<main id="main-content" class="center-state" aria-busy="true">
<p role="status">Loading fleet…</p>
</main>
{:else}
<div class="workspace-shell">
<aside class="fleet" aria-label="Fleet">
<div class="fleet-label">Fleet</div>
{#each hosts as item (item.id)}
<button class="owner-button" class:active={connectionId === item.id && yardName === null} aria-label={`Show owner ${item.snapshot?.owner.id ?? item.record?.hostId ?? 'local'}${item.record ? ` at ${item.record.destination}` : ''}`} aria-pressed={connectionId === item.id && yardName === null} on:click={() => select(item.id, null)}>
<span>
<strong>
{item.snapshot?.owner.id ?? item.record?.hostId ?? 'Local owner host'}
</strong>
<small>
{item.record?.destination ?? 'Local owner host'} {item.state ?? item.record?.state ?? ''}
</small>
</span>
</button>
{#if item.error}
<p class="fleet-empty">
{item.error}
</p>
{/if}{#if item.snapshot?.owner.yards.length === 0}
<p class="fleet-empty">No local yards found.</p>
{/if}
<ul class="yard-list">
{#each item.snapshot?.owner.yards ?? [] as y}
<li>
<button class:active={connectionId === item.id && yardName === y.name} aria-label={`Show yard ${y.name}`} aria-pressed={connectionId === item.id && yardName === y.name} on:click={() => select(item.id,y.name)}>
<span aria-hidden="true">└</span>
<span class="yard-copy">
<strong>
{y.name}
</strong>
<small>
{y.state}
</small>
</span>
</button>
</li>
{/each}
</ul>
{/each}
<button class="connect-link" on:click={() => {void releaseAssessments(); connecting = true;}}>+ Connect remote host</button>
</aside>
<main id="main-content" class="workspace">
{#if refreshing}
<p class="refreshing" role="status">Refreshing local fleet…</p>
{/if}
{#if error}
<div class="inline-error" role="alert">
{error}{#if incompatibleError || /incompatible|version|schema|capability/i.test(error)}
{#if errorCode !== 'incompatible_engine'}<p>Install Veranda and Subyard from the same release.</p>{/if}
{#if snapshot?.verandaVersion || snapshot?.engineVersion}
<p>Last known versions: Veranda: {snapshot?.verandaVersion ?? 'Unavailable'}; Subyard: {snapshot?.engineVersion ?? 'Unavailable'}.</p>
{/if}
{/if}
<button on:click={refresh}>Try again</button>
</div>
{/if}
{#if connecting}
<section>
<p class="eyebrow">New connection</p>
<h1>Connect a remote Subyard host</h1>
<p>All yards discovered through this owner host will appear in the fleet.</p>
{#if !assessment}
<form on:submit|preventDefault={() => assess()}>
<label for="destination">SSH destination</label>
<input id="destination" bind:value={destination} required placeholder="user@hostname" autocomplete="off" />
<p>Authentication: system SSH agent.</p>
<button class="primary" disabled={busy}>Test connection</button>
</form>
{:else}
<h2>Verify host key</h2>
<p>
{assessment.destination}
</p>
<code class="fingerprint">
{assessment.fingerprint}
</code>
{#if assessment.previousFingerprint}
<p>Previous fingerprint: {assessment.previousFingerprint}
</p>
{/if}
<p>Compare this fingerprint with the owner host before continuing.</p>
<ul>
{#each assessment.consequences as consequence}
<li>
{consequence}
</li>
{/each}
</ul>
<label>
<input type="checkbox" bind:checked={accepted} /> I verified and accept this host key</label>
<p>Next: negotiate RPC, discover yards, and save the connection.</p>
<button class="primary" disabled={busy || !accepted} on:click={connect}>Connect and save</button>
{/if}
<button disabled={busy} on:click={() => {connecting = false; void releaseAssessments();}}>Cancel connection</button>
</section>
{:else if snapshot}
<section>
<p class="eyebrow">
{yard ? `Yard · ${snapshot.owner.id}` : 'Owner host'}
</p>
<div class="object-heading">
<div>
<h1>
{yard?.name ?? snapshot.owner.id}
</h1>
<p>
{yard ? yard.state : `${snapshot.owner.yards.length} ${snapshot.owner.yards.length === 1 ? 'yard' : 'yards'}`}
</p>
</div>
<div class="actions">
{#if yard}
{#if yard.state === 'STOPPED'}<button disabled={busy || unavailable || !canMutate} on:click={() => prepare('start', [])}>Start yard</button>{/if}
{#if yard.state === 'RUNNING'}<button disabled={busy || unavailable || !canMutate} on:click={() => prepare('stop', [])}>Stop yard</button>{/if}
<button disabled={busy || unavailable || !canMutate} on:click={() => prepare('init', [])}>Reconcile yard</button>
{/if}
{#if host?.record && !yard}
<button disabled={busy} on:click={() => assess(host!.record!.id)}>Repair connection</button>
<button disabled={busy} on:click={() => assessRemoval(host!.record!.id)}>Remove connection</button>
{/if}
<button disabled={busy || unavailable || !canSession || !!loader} on:click={() => launch('shell')}>Shell</button>
<button disabled={busy || unavailable || !canSession || !!loader} on:click={() => copyShell()}>Copy shell command</button>
{#if !yard}
<button disabled={busy || unavailable || !canSession || !!loader} on:click={() => launch('resources')}>CPU / RAM terminal</button>
{/if}
</div>
</div>
{#if unavailable}
<div class="inline-error" role="alert">
{selectionState}. Refresh to reconnect before making changes.{#if selectionState === 'incompatible'}
<p>Install Veranda and Subyard from the same release. Last known versions: Veranda: {snapshot.verandaVersion ?? 'Unavailable'}; Subyard: {snapshot.engineVersion}.</p>
{/if}
</div>
{/if}
<nav class="tabs" aria-label={yard ? 'Yard sections' : 'Host sections'}>
{#each tabs as name}
<button class:selected={tab === name} aria-current={tab === name ? 'page' : undefined} on:click={() => {selectionGeneration++; tab = name; setting = null; remember();}}>
{name}
</button>
{/each}
</nav>
<div class="content-panel owner-summary">
{#if detailsLoading && !['Profiles', 'Settings', 'Diagnostics', 'Sync'].includes(tab)}<p role="status">Loading {yard ? 'yard' : 'host'} details…</p>{/if}
{#if detailsLoading && ['Profiles', 'Settings', 'Diagnostics', 'Sync'].includes(tab)}
<p role="status">Loading {yard ? 'yard' : 'host'} details…</p>
{:else if tab === 'Projects' && yard}
<h2>Projects</h2>
{#if connectionId !== null}<p>Remote VS Code needs an SSH agent key already authorized in this yard.</p>{/if}
{#if !yard.projects.length}
<h3>No projects in this yard</h3>
<p>Projects appear after the owner registers them.</p>
{:else}
<ul class="project-list" aria-label={`Projects in ${yard.name}`}>
{#each yard.projects as project}
<li>
<strong>
{project.name}
</strong>
<div class="actions">
<button disabled={busy || unavailable || !canSession || !!loader} on:click={() => launch('vscode',project.id)}>Open in VS Code</button>
<button disabled={busy || unavailable || !canSession || !!loader} on:click={() => launch('shell',project.id)}>Shell</button>
<button disabled={busy || unavailable || !canSession || !!loader} on:click={() => copyShell(project.id)}>Copy shell command for {project.name}</button>
</div>
</li>
{/each}
</ul>
{/if}
{:else if tab === 'Profiles'}
<h2>Profiles</h2>
{#if details.selection}<p>Effective selection: {details.selection.value || "None"}</p><ul aria-label="Profile selection provenance">{#each details.selection.provenance as source}<li>{source.scope} · {source.role} · {source.status}</li>{/each}</ul>{/if}
<p>Deselecting changes selection; it does not uninstall artifacts. Reconciliation is separate.</p>
{#if details.capabilities?.includes('profile-list-v1') && details.profiles}
<ul class="profile-list">
{#each details.profiles as profile}
<li>
<label>
<input type="checkbox" checked={profile.selected} disabled={busy || unavailable || !canMutate || !profile.eligible} on:change={() => prepare('config',['set','ENVIRONMENT_PROFILES',details.profiles!.filter(p => p.name === profile.name ? !p.selected : p.selected).map(p => p.name).join(' '),'--scope','yard'])} /> {profile.name}
</label>
<span>
{profile.convergence === 'current' ? 'Applied' : profile.convergence === 'not-applicable' ? 'Not applicable' : profile.convergence === 'unknown' ? 'Unavailable' : 'Available'}
</span>
<details>
<summary>Details</summary>
<p>
{profile.description ?? 'No description reported by the owner.'}
</p>
<p>Selected: {profile.selected ? 'Yes' : 'No'} · Provisioning scope: {profile.provisionScope}
</p>
{#if profile.provisionable}
<button disabled={busy || unavailable || !profile.eligible || !canMutate} on:click={() => prepare('provision',[profile.name])}>Reconcile profile</button>
{/if}
</details>
</li>
{/each}
</ul>
{:else}
<p>Profiles unavailable. Requires profile-list-v1.</p>
{/if}
{:else if tab === 'Settings'}
<h2>Settings</h2>
{#if details.capabilities?.includes('settings-list-v1') && details.settings}
<table>
<thead>
<tr>
<th>Setting</th>
<th>Effective value</th>
<th>Scope</th>
<th>Action</th>
</tr>
</thead>
<tbody>
{#each details.settings as row}
<tr>
<th>
{row.name}
</th>
<td>
{row.valueAvailable ? value(row.value) : 'Unavailable'}
</td>
<td>
{row.provenance.map(p => p.scope).join(', ')}
</td>
<td>
<button disabled={busy || unavailable || !canMutate || !row.editable} on:click={() => {setting = row; settingValue = row.valueAvailable ? String(row.value ?? '') : '';}}>Edit</button>
</td>
</tr>
{/each}
</tbody>
</table>
{:else}
<p>Settings unavailable. Requires settings-list-v1.</p>
{/if}
{#if setting}
<form on:submit|preventDefault={() => { if (canSettings) void prepare('config',['set',setting!.name,settingValue,'--scope',yard ? 'yard' : 'host']); }}>
<label for="setting-value">
{setting.name}
</label>
{#if setting.enum?.length}
<select id="setting-value" bind:value={settingValue}>
{#each setting.enum as choice}
<option>
{choice}
</option>
{/each}
</select>
{:else if setting.type === 'multiline' || setting.type.endsWith('-list')}
<textarea id="setting-value" bind:value={settingValue} rows="5" required></textarea>
{:else}
<input id="setting-value" bind:value={settingValue} type={setting.type === 'integer' ? 'number' : 'text'} min={setting.minimum} max={setting.maximum} required />
{/if}
<button disabled={busy || unavailable || !canSettings}>Preview change</button>
<button type="button" disabled={busy || unavailable || !canSettings} on:click={() => { if (canSettings) void prepare('config',['unset',setting!.name,'--scope',yard ? 'yard' : 'host']); }}>Preview unset</button>
<button type="button" on:click={() => setting = null}>Cancel edit</button>
</form>
{/if}
{:else if tab === 'Diagnostics'}
<h2>Diagnostics</h2>
{#if details.diagnostics?.length}
<dl>
{#each details.diagnostics as fact}
<dt>
{fact.label}
</dt>
<dd>
{fact.value}
</dd>
{/each}
</dl>
{:else}
<p>Typed diagnostics unavailable.</p>
{/if}
{:else if tab === 'Sync'}
<h2>Host synchronization</h2>
{#if !canSync}<p>Synchronization unavailable until typed owner status and operation support are negotiated.</p>{/if}
<div class="sync-cards">
<section>
<h3>Configuration repository</h3>
<p>Registration: {details.sync?.registration ?? 'Unavailable'}
</p>
<p>Repository status: {details.sync?.git?.available ? "Available" : "Unavailable"}</p><p>Remote: {details.sync?.git?.remote ?? "Unavailable"}</p>
<p>Branch: {details.sync?.git?.branch ?? 'Unavailable'}
</p>
<p>Relation: {details.sync?.git?.relation ?? 'Unavailable'}
</p>
<p>Worktree: {details.sync?.git?.worktree ?? 'Unavailable'}
</p>
<p>Applied commit: {details.sync?.appliedCommit ?? 'Unavailable'}
</p>
{#if details.sync?.registration === "configured"}
<div class="actions">
<button disabled={busy || unavailable || !canSync} on:click={() => prepare("config", ["sync", "pull", "--apply"])}>Preview pull</button>
<button disabled={busy || unavailable || !canSync} on:click={() => prepare("config", ["sync", "push", "--apply"])}>Preview push</button>
</div>
{:else}
<label>Configuration repository URL<input bind:value={repositoryUrl} placeholder="git@host:team/subyard-config.git" autocomplete="off" /></label>
<p>Use an SSH or HTTPS URL without credentials, query parameters or fragments. Authentication uses the owner's Git configuration.</p>
<button disabled={busy || unavailable || !canSync || !repositoryUrl.trim()} on:click={() => prepare('config', ['sync', 'connect', repositoryUrl.trim()], null)}>Preview repository connection</button>
{/if}
</section>
<section>
<h3>Credentials</h3>
<p>State: {details.sync?.credentials.state ?? 'Unavailable'}
</p>
<p>Records: {details.sync?.credentials.records ?? 'Unavailable'} · Conflicts: {details.sync?.credentials.conflicts ?? 'Unavailable'}
</p>
<ul aria-label="Credential peers">{#each details.sync?.credentials.peers ?? [] as peer}<li><strong>{peer.name}</strong> · {peer.role} · {peer.trusted ? "Trusted" : "Untrusted"}<p>Last success: {timestamp(peer.lastSuccess)}</p><p>Last attempt: {timestamp(peer.lastAttempt)} · {peer.failed ? "Failed" : "No failure reported"} · Consecutive failures: {peer.consecutiveFailures}</p><p>{peer.manualOnly ? "Manual only" : "Owner-managed retry"}</p></li>{/each}</ul>
<p>Only redacted metadata is shown. Synchronization is manual.</p>
<button disabled={busy || unavailable || !canSync} on:click={() => prepare("keys", ["sync", "--now"])}>Sync now</button>
</section>
</div>
{:else if tab === 'Yards'}
<h2>Yards on this host</h2>
<button disabled={busy || unavailable || !canCreateYard} on:click={() => {creatingYard = true; newYardProfile = yardPresets[0]?.name ?? '';}}>Create yard</button>
{#if !detailsLoading && !canCreateYard}<p>Yard creation requires owner support for yard-bootstrap-v1 and an available yard preset.</p>{/if}
{#if creatingYard}
<form on:submit|preventDefault={() => {if (canCreateYard && newYard !== 'default' && /^[a-z0-9][a-z0-9_-]{0,127}$/.test(newYard) && yardPresets.some(p => p.name === newYardProfile)) void prepare("init", ["--profile", newYardProfile], newYard);}}>
<label for="new-yard">Yard name</label>
<input id="new-yard" bind:value={newYard} required maxlength="128" pattern={'[a-z0-9][a-z0-9_-]{0,127}'} />
<p>Use a non-default name with lowercase ASCII letters, digits, underscores or hyphens.</p>
<label for="new-yard-profile">Yard preset</label>
<select id="new-yard-profile" bind:value={newYardProfile}>{#each yardPresets as preset}<option value={preset.name}>{preset.name}</option>{/each}</select>
<button disabled={busy || !canCreateYard || newYard === 'default'}>Preview creation</button>
<button type="button" on:click={() => creatingYard = false}>Cancel creation</button>
</form>
{/if}
<ul>
{#each snapshot.owner.yards as y}
<li>
<button on:click={() => select(connectionId,y.name)}>
{y.name}
</button> {y.state}
</li>
{/each}
</ul>
{:else}
<h2>Overview</h2>
<p>
{yard ? `${yard.projects.length} registered projects` : 'Select a yard to inspect registered projects.'}
</p>
<p>Subyard version: {snapshot.engineVersion}
</p>
{/if}
</div>
</section>
<footer>Inventory observed <time datetime={snapshot.observedAt}>
{snapshot.observedAt}
</time>
</footer>
{:else}
<h1>Fleet unavailable</h1>
<p>Select a host or connect a remote host.</p>
{#if host?.record}
<button disabled={busy} on:click={() => assess(host!.record!.id)}>Repair connection</button>
<button on:click={() => assessRemoval(host!.record!.id)}>Remove connection</button>
{/if}{/if}
{#if removal}
<section class="operation-panel">
<h2>Remove saved connection?</h2>
<p>
{removal.connection.destination}
</p>
<ul>
{#each removal.consequences as consequence}
<li>
{consequence}
</li>
{/each}
</ul>
<button disabled={busy} on:click={() => guarded(async () => {await api.removeConnection({assessmentId: removal!.assessmentId, confirmed: true}); removal = null; connectionId = null; await refresh();})}>Confirm removal</button>
<button on:click={() => releaseAssessments()}>Keep connection</button>
</section>
{/if}{#if !loader && snapshot && !canSession}<p>Shell, VS Code and resource sessions require owner session support. Install Veranda and Subyard from the same release.</p>{/if}
{#if sessionCommand}
<section class="operation-panel">
<h2>Session command</h2>
<label for="session-command">Shell command</label>
<textarea id="session-command" readonly value={sessionCommand} rows="3"></textarea>
{#if copyHint}<p role="status">{copyHint}</p>{/if}
<p>Use this command in your terminal if a terminal window could not be opened.</p>
</section>
{/if}{#if plan}
<section class="operation-panel" aria-labelledby="plan-title">
<h2 id="plan-title">Review owner plan</h2><p>Target: {planTarget}</p>
<p>
{plan.summary}
</p>
<ul>
{#each plan.consequences as consequence}
<li>
{consequence}
</li>
{/each}
</ul>
<ol>
{#each plan.steps as step}
<li>
<strong>
{step.target} · {step.decision}
</strong>
<dl>
<dt>Observed</dt>
<dd>
{step.observed}
</dd>
<dt>Desired</dt>
<dd>
{step.desired}
</dd>
<dt>Preconditions</dt>
<dd>
{step.preconditions.join('; ') || 'None'}
</dd>
<dt>Dependencies</dt>
<dd>
{step.dependsOn.join(', ') || 'None'}
</dd>
<dt>Verification</dt>
<dd>
{step.verify}
</dd>
</dl>
{#if step.consequence}
<p>
{step.consequence}
</p>
{/if}
</li>
{/each}
</ol>
<p>Expires: {plan.expiresAt}
</p>
<p>Plan digest: <code>
{plan.digest}
</code>
</p>
{#if plan.confirmation !== 'never'}
<label>
<input type="checkbox" bind:checked={accepted} /> I confirm this exact plan</label>
{/if}
<div class="actions">
<button class="primary" disabled={busy || unavailable || Date.parse(plan.expiresAt) <= Date.now() || (plan.confirmation !== 'never' && !accepted)} on:click={execute}>Apply plan</button>
<button disabled={busy} on:click={discard}>Discard plan</button>
</div>
</section>
{/if}
{#if operationId}
<section class="operation-panel" aria-labelledby="operation-title">
<h2 id="operation-title">Operation</h2>
<p role="status">
{operationState}
</p>
<ol class="operation-log">
{#each messages as message}
<li>
{message}
</li>
{/each}
</ol>
{#if operationState === 'running'}
<button disabled={busy} on:click={() => guarded(async () => {await api.cancelOperation({operationId}); operationState = 'cancellation requested';})}>Cancel operation</button>
{/if}
</section>
{/if}
</main>
</div>
{/if}
</div>
