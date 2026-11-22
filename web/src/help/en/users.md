# Users

**Users and groups** — who can use the NAS. One account is used for folders over the network (SMB),
the web **Files** section, SSH sign-in and — for administrators — signing in to this interface;
the password is the same for all of them.

## What you see
- The user list with the marks **administrator**, **disabled**, **password not synced — set it again**.
  The **SSH sign-in** column — on/off.
- **New user**: **Name (Latin letters)**, **Password (web and Samba)**, the **Administrator**
  (signs in to this interface) and **SSH sign-in** (by default — administrators only) options.
- For a user: **Change** password, **Disable** / **Enable**, **Delete**.
- Groups: the built-in **administrators** and **users**, plus your own — **New group**. **Members** —
  who belongs to the group.

## How it works
- Members of the **administrators** group can sign in to the interface; a new user is added to the
  **users** group.
- A disabled user cannot sign in to the interface or to folders over the network; the account and
  files are kept.
- The last administrator cannot be deleted; at least one administrator always keeps SSH sign-in.
- Folder permissions are granted in [Shared folders](help:shares) — to users or groups.
- After a password change the user is signed out of all their interface sessions and signs in again;
  already open SSH sessions are not terminated.

## What not to do
- A user's files on the pools are kept when the account is deleted — delete them yourself if they are
  no longer needed.
- Do not grant administrator rights “just in case”: an administrator can delete a pool.

See also: [Shared folders](help:shares), [Settings](help:settings).
