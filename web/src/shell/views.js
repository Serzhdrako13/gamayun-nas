// Содержимое разделов по ключам манифеста.
import Pools from '../routes/Pools.svelte';
import Disks from '../routes/Disks.svelte';
import Shares from '../routes/Shares.svelte';
import Users from '../routes/Users.svelte';
import Journal from '../routes/Journal.svelte';
import Settings from '../routes/Settings.svelte';
import Notifications from '../routes/Notifications.svelte';
import Channels from '../routes/Channels.svelte';
import ConfigBackup from '../routes/ConfigBackup.svelte';
import Files from '../routes/Files.svelte';
import Help from '../routes/Help.svelte';
import Apps from '../routes/Apps.svelte';
import AppsPorts from '../routes/AppsPorts.svelte';
import AppsCatalog from '../routes/AppsCatalog.svelte';

export const VIEWS = { pools: Pools, disks: Disks, shares: Shares, users: Users, journal: Journal, settings: Settings, notifications: Notifications, channels: Channels, confbackup: ConfigBackup, files: Files, help: Help, apps: Apps, appsPorts: AppsPorts, appsCatalog: AppsCatalog };
