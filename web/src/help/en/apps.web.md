# App web address

An app with a web interface can be opened at its own address on the NAS, for example
`https://jellyfin-<nas-name>.local/`, without a port number. Sign-in uses your NAS administrator
account: a stranger on the network will not get into the app, even if the app itself has no password.

## What you see
- On the app card — **Web address**: the name (the beginning of the address before the NAS name) and
  the **App port**. Only an app port available "on this NAS only" (127.0.0.1, TCP) is suitable.
- **Publish** — the address appears immediately. **Unpublish** — the address stops opening; the app keeps
  running.
- **the NAS root certificate** — a file to install on a computer or phone so the browser trusts app
  addresses.

## How it works
1. A proxy runs on the NAS (the `gamayun-caddy` service, installed together with the app subsystem).
   It accepts requests on port 443 and passes them to the app.
2. The first time you open an app address, you get the NAS sign-in page. If you are already signed in,
   this step passes by itself in a fraction of a second. The app does not receive your NAS sign-in, only
   its own.
3. Signing out of the NAS, changing the password or disabling the account also closes access to apps.
   Using an app extends your NAS session.
4. Certificates for app addresses are issued by the NAS itself. Until the root certificate is installed,
   the browser shows a warning.

## What to do
- **Install the root certificate.** Download it on the app card and open the file: in Windows —
  "Install Certificate" → "Local Machine" → the "Trusted Root Certification Authorities" store; in
  macOS — "Keychain Access" → "Always Trust"; in Android and iOS — via security settings. There is one
  certificate per NAS, installed once.
- **The address does not open.** The NAS announces `*.local` names via mDNS (Bonjour), like its own
  name. If a device does not see the address, add it to the router's DNS or to the hosts file with the
  NAS address.
- **"The app proxy is not installed or not running".** Check the service: `systemctl status gamayun-caddy`.
- **A mobile app (Jellyfin, Immich) cannot sign in.** Such apps cannot handle the NAS sign-in page.
  For them, make the port "available on the network" in the app settings.
