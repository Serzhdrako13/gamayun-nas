<script>
  // Реестр портов NAS.
  import { onMount } from 'svelte';
  import { get } from '../lib/api.js';
  import HelpButton from '../help/HelpButton.svelte';
  import { t } from '../lib/i18n/index.js';

  let ports = $state(null);
  let err = $state('');
  async function load() {
    try {
      ports = (await get('/apps/ports')).ports || [];
      err = '';
    } catch (e) {
      err = e.message;
    }
  }
  onMount(load);
</script>

<div class="stack">
  <section class="card stack">
    <div class="row between">
      <h2>{t('apps.ports.title')} <HelpButton section="apps.ports" /></h2>
      <button onclick={load}>{t('common.refresh')}</button>
    </div>
    {#if err}<p class="error" role="alert">{err}</p>{/if}
    <div class="tablewrap">
      <table>
        <thead><tr><th>{t('apps.ports.col.port')}</th><th>{t('apps.ports.col.owner')}</th><th>{t('apps.ports.col.access')}</th></tr></thead>
        <tbody>
          {#each ports || [] as p}
            <tr>
              <td>{p.port}/{p.proto}</td>
              <td>{t('apps.ports.owner.' + p.owner)}{#if p.title} — {p.title}{/if}{#if p.app} — {p.app}{#if p.service} ({p.service}){/if}{/if}</td>
              <td>{#if p.owner === 'app'}{p.network ? t('apps.inNetwork') : t('apps.localOnly')}{:else}—{/if}</td>
            </tr>
          {:else}
            <tr><td colspan="3" class="muted">{ports ? '—' : t('common.loading')}</td></tr>
          {/each}
        </tbody>
      </table>
    </div>
  </section>
</div>
