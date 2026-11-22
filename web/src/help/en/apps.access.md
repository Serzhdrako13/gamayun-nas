# Account and folder access

Each app has its own account on the NAS — like a separate key given to a plumber: it opens only the door
he needs. The app sees its own data and the shared folders you granted access to, and nothing else.
The permissions of NAS users do not change.

## What you see
- **Runs as** — for each service of the app:
  - **own account (recommended)** — the program in the container runs as the account `app-<identifier>`;
  - **own account via PUID/PGID** — for images that switch to the right account themselves (for example,
    linuxserver.io images): the container starts with full privileges, then the program continues as
    the app account;
  - **as root (unrestricted)** — for images that do not work otherwise. Folder permissions do not apply
    to such a service: only a read-only mount protects the folder.
  The mode is chosen automatically from the compose file; change it if the app does not start.
- **Shared folder access** — every folder the compose file mounts into the app: **read only** (default)
  or **read and write**.
- On the app card — **Account and folder access**: the account name, its number (UID), granted folders,
  and who granted them and when.
- **root access** — a badge on apps added before app accounts existed.

## How it works
1. When an app is created, its account is created too, and the app data is handed over to it.
2. Permissions for this account only are set on the selected folders (on all nested files and folders).
   In a large folder this takes time — progress is shown in the task.
3. New files the app creates in a folder with write access belong to the app account, and their group
   is the common group of NAS users with write access. So NAS users see them and can change them over
   the network as usual. In Windows the owner is shown as "Unix User\app-…".
4. If you remove access, the account's permissions are removed from the whole folder. Files the app has
   already created stay in place.
5. Every 10 minutes the NAS checks the permissions against the granted ones. If they have drifted (for
   example, the folder was restored from a snapshot), they are restored automatically and an event is
   written to the log.

## What to do
- The app does not start under its own account — open **Edit**, choose **own account via PUID/PGID**
  or, if that does not help, **as root**, and click **Check**.
- **root access** badge — open the app and click **Move to own account**. Folder access is offered as
  it was: folders mounted without read-only get write access. If the move fails, everything is restored.

## What not to do
- Do not grant write access to a folder if the app only needs to read: a media server does not need
  write access to play movies, and a compromised app with write access can damage files.

See also: [Add app](help:apps.add), [Risks](help:apps.risks).

:::details For administrators
The app account is `app-<id>` with a number (UID = GID) from the range 59000–59999, with no login and no
home directory; the number of a removed app is never reused. Folder permissions are POSIX ACLs (check
with `getfacl <folder>`); symbolic links are not followed when granting permissions. The owner of files
in shared folders is not changed.
:::
