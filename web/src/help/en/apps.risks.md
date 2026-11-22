# Risks

Some compose settings give an app more power than it usually needs. This is not always a mistake: a
media server, for example, needs the graphics card. So such settings are not forbidden, but each one
must be confirmed with a separate checkbox. The log records who confirmed which risk and when.

## What you see
The **Risks — confirm each one** block in the wizard and **Confirmed risks** on the app card.

| Risk | What it means |
|---|---|
| full access to the NAS system (privileged) | the app can do anything a NAS administrator can |
| access to Docker control (docker.sock) | the app can start any containers — the same as administrator rights |
| NAS network directly (network_mode: host) | the app sees all NAS network services and takes ports without a check |
| NAS processes, memory, host name (pid, ipc, uts: host), no user isolation | the app sees and can affect NAS programs |
| extra kernel capabilities (cap_add) | extended privileges inside the system |
| direct access to NAS devices (devices, gpus) | for example, to a disk or a graphics card |
| kernel protection disabled (unconfined) | removes the restrictions that contain a compromised app |
| the service runs as root and can change files in a shared folder | for "as root" mode with write access: folder permissions do not restrict it |
| access to a shared folder with root privileges | only apps added before app accounts existed; move them to their own account |
| port open to the whole network | see below |

**Port open to the whole network.** A port published by Docker goes **around the NAS firewall**: any
computer on your network can connect to it. Turn on **available on the network** only if the app is
used from other devices, and set a password in the app.

## What to do
1. Read each risk and ask yourself why this app needs it. The answer is usually in the program's
   description.
2. Not sure — do not confirm: remove the line from compose and click **Check** again.

## What not to do
- Do not confirm risks in bulk without reading: an app with Docker access or privileged mode, if
  compromised, gets the whole NAS along with the data.

See also: [Add app](help:apps.add), [Account and folder access](help:apps.access), [Ports](help:apps.ports).
