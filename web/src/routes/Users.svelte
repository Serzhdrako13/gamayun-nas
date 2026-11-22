<script>
  import HelpButton from '../help/HelpButton.svelte';
  import { onMount } from 'svelte';
  import { get, post, put, del } from '../lib/api.js';
  import { flash } from '../lib/state.svelte.js';
  import Drawer from '../lib/Drawer.svelte';
  import Confirm from '../lib/Confirm.svelte';
  import Switch from '../lib/Switch.svelte';
  import { t } from '../lib/i18n/index.js';

  let users = $state([]);
  let groups = $state([]);
  let err = $state('');
  let nu = $state({ name: '', password: '', admin: false, ssh: false });
  let sshTouched = $state(false);
  let ng = $state('');
  let pwUser = $state(null);
  let pw = $state('');
  let grp = $state(null);
  let members = $state([]);

  async function load() {
    try {
      [users, groups] = await Promise.all([get('/nas/users').then((r) => r.users || []), get('/nas/groups').then((r) => r.groups || [])]);
    } catch (e) {
      err = e.message;
    }
  }
  onMount(load);

  async function act(fn, ok) {
    err = '';
    try {
      await fn();
      if (ok) flash(ok);
      await load();
    } catch (e) {
      // 409 на SSH: у включённых администраторов хотя бы один должен сохранить вход по SSH.
      err = e.status === 409 && /SSH/.test(e.message) ? e.message + t('users.sshKeepHint') : e.message;
    }
  }
  const enc = encodeURIComponent;
  const addUser = (e) => {
    e.preventDefault();
    act(async () => {
      await post('/nas/users', { name: nu.name, password: nu.password, admin: nu.admin, ssh: sshTouched ? nu.ssh : nu.admin });
      nu = { name: '', password: '', admin: false, ssh: false };
      sshTouched = false;
    }, t('users.userCreated'));
  };
  const addGroup = (e) => {
    e.preventDefault();
    act(async () => {
      await post('/nas/groups', { name: ng });
      ng = '';
    }, t('users.groupCreated'));
  };
  const setPw = () =>
    act(async () => {
      await put(`/nas/users/${enc(pwUser)}/password`, { password: pw });
      pwUser = null;
      pw = '';
    }, t('users.passwordChanged'));
  const saveMembers = () =>
    act(async () => {
      await put(`/nas/groups/${enc(grp.name)}/members`, { members });
      grp = null;
    }, t('users.membersSaved'));
  function openGroup(g) {
    grp = g;
    members = [...(g.members || [])];
  }
</script>

{#if err}<p class="error" role="alert">{err}</p>{/if}
<div class="grid">
  <section class="card stack">
    <h2>{t('users.title')} <HelpButton section="users" /></h2>
    <div class="tablewrap">
      <table class="responsive">
        <thead><tr><th>{t('common.name')}</th><th>{t('common.groups')}</th><th>SSH</th><th></th></tr></thead>
        <tbody>
          {#each users as u}
            <tr>
              <td>{u.name} {#if u.admin}<span class="badge">{t('users.badge.admin')}</span>{/if} {#if !u.enabled}<span class="badge warn">{t('users.badge.disabled')}</span>{/if} {#if u.password_sync_error}<span class="badge warn" title={u.password_sync_error}>{t('users.badge.passwordUnsynced')}</span>{/if}</td>
              <td>{(u.groups || []).join(', ') || '—'}</td>
              <td>
                <span class="row" title={t('users.sshHint')}>
                  <Switch bare label={'SSH: ' + u.name} checked={u.ssh} onchange={(v) => act(() => put(`/nas/users/${enc(u.name)}/ssh`, { ssh: v }))} />
                  {u.ssh ? t('users.on') : t('users.off')}
                </span>
              </td>
              <td class="row">
                <button onclick={() => (pwUser = u.name)}>{t('common.password')}</button>
                <button onclick={() => act(() => put(`/nas/users/${enc(u.name)}/enabled`, { enabled: !u.enabled }))}>{u.enabled ? t('users.disable') : t('users.enable')}</button>
                <Confirm label={t('common.delete')} onconfirm={() => act(() => del(`/nas/users/${enc(u.name)}`), t('users.userDeleted'))} />
              </td>
            </tr>
          {:else}
            <tr><td colspan="4" class="muted">{t('users.noUsers')}</td></tr>
          {/each}
        </tbody>
      </table>
    </div>
  </section>

  <section class="card stack">
    <form onsubmit={addUser} class="stack">
      <h2>{t('users.newUser')}</h2>
      <label class="field"><span>{t('users.nameLatin')}</span><input bind:value={nu.name} autocomplete="off" required /></label>
      <label class="field"><span>{t('users.passwordWebSamba')}</span><input type="password" bind:value={nu.password} autocomplete="new-password" required /></label>
      <Switch label={t('users.admin')} hint={t('users.adminHint')} checked={nu.admin} onchange={(v) => (nu.admin = v)} />
      <Switch label={t('users.sshLogin')} hint={t('users.sshLoginHint')} checked={sshTouched ? nu.ssh : nu.admin} onchange={(v) => { nu.ssh = v; sshTouched = true; }} />
      <button class="primary">{t('common.create')}</button>
    </form>
  </section>

  <section class="card stack">
    <h2>{t('common.groups')}</h2>
    {#each groups as g}
      <div class="row between">
        <span><b>{g.name}</b> <span class="muted small">{(g.members || []).join(', ') || t('users.empty')}</span></span>
        <span class="row">
          <button onclick={() => openGroup(g)}>{t('users.members')}</button>
          {#if g.system}<span class="badge">{t('users.builtinGroup')}</span>{:else}
            <Confirm label={t('common.delete')} onconfirm={() => act(() => del(`/nas/groups/${enc(g.name)}`), t('users.groupDeleted'))} />
          {/if}
        </span>
      </div>
    {:else}
      <p class="muted">{t('users.noGroups')}</p>
    {/each}
    <form onsubmit={addGroup} class="row">
      <input bind:value={ng} placeholder={t('users.newGroup')} required style:flex="1" />
      <button class="primary">{t('common.create')}</button>
    </form>
  </section>
</div>

{#if pwUser}
  <Drawer title={t('users.passwordTitle', { name: pwUser })} onclose={() => (pwUser = null)}>
    <label class="field"><span>{t('common.newPassword')}</span><input type="password" bind:value={pw} autocomplete="new-password" /></label>
    <button class="primary" disabled={!pw} onclick={setPw}>{t('users.change')}</button>
  </Drawer>
{/if}
{#if grp}
  <Drawer title={t('users.membersTitle', { name: grp.name })} onclose={() => (grp = null)}>
    <div class="stack">
      {#each users as u}
        <label class="row"><input type="checkbox" value={u.name} bind:group={members} /> {u.name}</label>
      {:else}
        <p class="muted">{t('users.createUsersFirst')}</p>
      {/each}
      <button class="primary" onclick={saveMembers}>{t('common.save')}</button>
    </div>
  </Drawer>
{/if}
