<script lang="ts">
  import { onMount } from 'svelte';
  import './app.css';
  import { loadLocalFleet } from './native';
  import type { FleetLoader, LocalFleetError, LocalFleetSnapshot } from './types';

  export let loader: FleetLoader = loadLocalFleet;

  let snapshot: LocalFleetSnapshot | null = null;
  let selectedYardName: string | null = null;
  let loading = true;
  let refreshing = false;
  let error: LocalFleetError | null = null;

  $: selectedYard =
    snapshot?.owner.yards.find((yard) => yard.name === selectedYardName) ?? null;

  onMount(() => {
    void refresh();
  });

  function normalizeError(reason: unknown): LocalFleetError {
    if (
      typeof reason === 'object' &&
      reason !== null &&
      'message' in reason &&
      typeof reason.message === 'string'
    ) {
      return {
        code: 'code' in reason && typeof reason.code === 'string' ? reason.code : 'local_fleet_failed',
        message: reason.message
      };
    }
    return {
      code: 'local_fleet_failed',
      message: 'Veranda could not load the local fleet. Check that Yard is installed and try again.'
    };
  }

  async function refresh(): Promise<void> {
    if (snapshot) refreshing = true;
    else loading = true;
    error = null;

    try {
      const next = await loader();
      snapshot = next;
      const previousIsAvailable = next.owner.yards.some((yard) => yard.name === selectedYardName);
      const currentIsAvailable = next.owner.yards.some(
        (yard) => yard.name === next.currentYardName
      );
      if (!previousIsAvailable) {
        selectedYardName = currentIsAvailable
          ? next.currentYardName
          : (next.owner.yards[0]?.name ?? null);
      }
    } catch (reason) {
      error = normalizeError(reason);
    } finally {
      loading = false;
      refreshing = false;
    }
  }
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
    <button class="refresh" type="button" on:click={refresh} disabled={loading || refreshing}>
      <span aria-hidden="true">↻</span>
      Refresh local fleet
    </button>
  </header>

  {#if loading}
    <main id="main-content" class="center-state" aria-busy="true">
      <div class="loader" aria-hidden="true"></div>
      <p role="status">Loading local yards…</p>
    </main>
  {:else if error && !snapshot}
    <main id="main-content" class="center-state">
      <div class="error-mark" aria-hidden="true">!</div>
      <div role="alert">
        <h1>Local fleet unavailable</h1>
        <p>{error.message}</p>
      </div>
      <button class="primary" type="button" on:click={refresh}>Try again</button>
    </main>
  {:else if snapshot}
    <div class="workspace-shell">
      <aside class="fleet" aria-label="Local fleet">
        <div class="fleet-label">
          <span>Fleet</span>
          <span class="read-only">Read only</span>
        </div>

        <button
          class:active={selectedYardName === null}
          class="owner-button"
          type="button"
          aria-label={`Show owner ${snapshot.owner.id}`}
          aria-pressed={selectedYardName === null}
          on:click={() => (selectedYardName = null)}
        >
          <span class="owner-icon" aria-hidden="true">⌂</span>
          <span>
            <strong>{snapshot.owner.id}</strong>
            <small>Local owner host</small>
          </span>
        </button>

        {#if snapshot.owner.yards.length === 0}
          <p class="fleet-empty">No local yards found.</p>
        {:else}
          <ul class="yard-list" aria-label={`Yards on ${snapshot.owner.id}`}>
            {#each snapshot.owner.yards as yard (yard.name)}
              <li>
                <button
                  class:active={selectedYardName === yard.name}
                  type="button"
                  aria-label={`Show yard ${yard.name}`}
                  aria-pressed={selectedYardName === yard.name}
                  on:click={() => (selectedYardName = yard.name)}
                >
                  <span class="branch" aria-hidden="true">└</span>
                  <span class="yard-copy">
                    <strong>{yard.name}</strong>
                    <small>{yard.kind}</small>
                  </span>
                  <span class="state-dot" data-state={yard.state} aria-hidden="true"></span>
                  <span class="sr-only">{yard.state}</span>
                </button>
              </li>
            {/each}
          </ul>
        {/if}

        <div class="engine-note">
          <span>Yard engine</span>
          <code>{snapshot.engineVersion}</code>
        </div>
      </aside>

      <main id="main-content" class="workspace">
        {#if refreshing}
          <p class="refreshing" role="status">Refreshing local fleet…</p>
        {/if}
        {#if error}
          <div class="inline-error" role="alert">
            <strong>Refresh failed.</strong> {error.message}
          </div>
        {/if}

        {#if selectedYard}
          <section aria-labelledby="yard-title">
            <p class="eyebrow">Yard · {snapshot.owner.id}</p>
            <div class="object-heading">
              <div>
                <h1 id="yard-title">{selectedYard.name}</h1>
                <p>
                  <span class="state-label">{selectedYard.state}</span>
                  <span aria-hidden="true">·</span>
                  {selectedYard.projects.length}
                  {selectedYard.projects.length === 1 ? 'project' : 'projects'}
                </p>
              </div>
              <span class="kind-chip">{selectedYard.kind}</span>
            </div>

            <nav class="tabs" aria-label="Yard sections">
              <span>Overview</span>
              <span class="selected" aria-current="page">Projects</span>
            </nav>

            <div class="content-panel">
              <div class="panel-heading">
                <div>
                  <p class="eyebrow">Read-only inventory</p>
                  <h2>Projects</h2>
                </div>
                <p>Names reported by the owner-side registry.</p>
              </div>

              {#if selectedYard.projects.length === 0}
                <div class="empty-state">
                  <span aria-hidden="true">◇</span>
                  <h3>No projects in this yard</h3>
                  <p>Projects will appear after Yard registers them on the owner host.</p>
                </div>
              {:else}
                <ul class="project-list" aria-label={`Projects in ${selectedYard.name}`}>
                  {#each selectedYard.projects as project (project.id)}
                    <li>
                      <span class="project-icon" aria-hidden="true">/</span>
                      <span>
                        <strong>{project.name}</strong>
                        <small>Project</small>
                      </span>
                    </li>
                  {/each}
                </ul>
              {/if}
            </div>
          </section>
        {:else}
          <section aria-labelledby="owner-title">
            <p class="eyebrow">Local owner host</p>
            <div class="object-heading">
              <div>
                <h1 id="owner-title">{snapshot.owner.id}</h1>
                <p>
                  {snapshot.owner.yards.length}
                  {snapshot.owner.yards.length === 1 ? 'yard' : 'yards'}
                </p>
              </div>
              <span class="kind-chip">Local</span>
            </div>
            <div class="content-panel owner-summary">
              <p class="eyebrow">Overview</p>
              <h2>Yards on this host</h2>
              <p>Select a yard from the fleet to inspect its registered project names.</p>
            </div>
          </section>
        {/if}

        <footer>
          Inventory observed <time datetime={snapshot.observedAt}>{snapshot.observedAt}</time>
        </footer>
      </main>
    </div>
  {/if}
</div>
