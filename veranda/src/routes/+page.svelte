<script lang="ts">
  import { onMount } from 'svelte';
  import FleetApp from '$lib/FleetApp.svelte';
  import { reportFrontendDiagnostic } from '$lib/native';

  onMount(() => {
    const error = () => reportFrontendDiagnostic('error');
    const rejection = () => reportFrontendDiagnostic('unhandled_rejection');
    window.addEventListener('error', error);
    window.addEventListener('unhandledrejection', rejection);
    return () => {
      window.removeEventListener('error', error);
      window.removeEventListener('unhandledrejection', rejection);
    };
  });
</script>

<svelte:head>
  <title>Subyard Veranda</title>
  <meta
    name="description"
    content="A clear, read-only view of the yards and projects on this Subyard host."
  />
</svelte:head>

<svelte:boundary onerror={() => reportFrontendDiagnostic('boundary')}>
  <FleetApp />
  {#snippet failed(_error, reset)}
    <main class="center-state" role="alert">
      <h1>Veranda could not display the fleet</h1>
      <p>Try loading the fleet again. If this continues, close and reopen Veranda.</p>
      <button onclick={reset}>Reload fleet</button>
    </main>
  {/snippet}
</svelte:boundary>
