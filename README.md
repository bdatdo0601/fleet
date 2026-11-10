# fleet

A self-hosted [Nebula](https://github.com/slackhq/nebula) network for my machines and phone.
Each device is registered once, by hand. After that, any admin device can SSH or
screen-share into any machine, from home, the office or anywhere else, with no third-party
servers involved.

```
 iPhone / iPad (admin)      Mobile Nebula app ─┐
 laptop (admin)             bin/apply ─────────┤
 office Mac (managed)       bin/apply ─────────┼──► Mac mini at home: lighthouse + relay + CA
 ...                                           │    UDP 443 port-forwarded on the router
                                               │
   devices connect to each other directly; if blocked by NAT/firewalls, through the mini
   (still end-to-end encrypted: the mini can't read relayed traffic)
```

| File | Purpose |
|---|---|
| `hosts.conf` | The one list of devices: name, mesh IP, kind, groups, cert lifetime |
| `blocklist.conf` | Revoked certificate fingerprints |
| `nebula/*.yml.tmpl` | Config for the lighthouse and for every other Mac |
| `pki/ca.crt`, `pki/certs/`, `pki/requests/` | Public certs and signing requests. **No private keys are ever stored here** (`*.key` is gitignored) |
| `bin/` | `ca-init`, `apply`, `sign`, `revoke`, `ssh-harden`, `check` |
| `shell/gauth.zsh` | `gauth`: gcloud login from a phone (see [Use](#use)) |

Access policy (set in the templates): devices in the **admin** group may connect to any
machine on TCP 22 (SSH) and 5900 (Screen Sharing), and ping works between all devices.
Everything else is dropped. **managed** machines accept connections from admins but
can't initiate any.

## One-time home setup

1. **Router:** forward **UDP 443** to the Mac mini, and reserve the mini's LAN IP.
   Make sure your ISP doesn't use CGNAT: the router's WAN IP should equal `curl ifconfig.me`.
2. **DDNS:** give the home connection a hostname (router DDNS, or a Cloudflare API script).
3. **Mac mini:**
   ```bash
   git clone git@github.com:bdatdo0601/fleet.git && cd fleet
   cp local.env.example local.env      # NODE_NAME=mini, fill in the rest
   bin/ca-init                         # passphrase-protected CA key in ~/.fleet-ca
   bin/apply                           # signs its own cert, installs, starts nebula
   git add pki && git commit -m "fleet: CA + mini" && git push
   ```
   In Energy settings, enable "Prevent automatic sleeping" and "Start up automatically after a power failure".
   Back up `~/.fleet-ca/ca.key` (e.g. in 1Password).

## Register a Mac (laptop, office machine, ...)

1. Add it to `hosts.conf` (unique IP; `admin` if you'll control other machines *from*
   it, `managed` otherwise). Commit and push.
2. On that Mac:
   ```bash
   git clone git@github.com:bdatdo0601/fleet.git && cd fleet
   cp local.env.example local.env      # NODE_NAME=<its name>
   bin/apply                           # creates its key + pki/requests/<name>.pub
   git add pki/requests && git commit -m "fleet: request <name>" && git push
   ```
3. On the mini: `git pull && bin/sign <name> && git add pki/certs && git commit -m "fleet: sign <name>" && git push`
4. On the Mac: `git pull && bin/apply`, then `ping 10.213.0.1`.
5. To reach it over SSH: turn on Remote Login in System Settings, run `bin/ssh-harden`
   (key-only logins, mesh only), and add your admin devices' SSH public keys to `~/.ssh/authorized_keys`.
   For screen sharing, turn on Screen Sharing and limit it to your user.

## Register a phone / iPad

1. Install **Mobile Nebula** (Defined Networking) and add a site. The app generates its own key.
2. Save the **public key** the app shows to `pki/requests/<name>.pub` on the mini, then run `bin/sign <name>`.
3. In the app, paste `pki/certs/<name>.crt` as the certificate and `pki/ca.crt` as the CA.
   Add a host: `10.213.0.1`, mark it as a **lighthouse**, with address `<DDNS name>:443`.
4. Add an SSH client (Blink, Termius), create a key, and authorize it on the machines you'll manage.

## Use

- SSH: `ssh datdo@10.213.0.10`. Screen sharing: VNC to `10.213.0.10:5900` (Screens, Jump Desktop, ...).
- gcloud login from a phone: `source shell/gauth.zsh` in `~/.zshrc` on the target Mac, then
  run `gauth` over SSH. Copy the URL into Safari, sign in, and paste the code back. If the
  connection drops, run `gauth` again to reattach.

## Change or revoke

- **Change config or templates:** edit, run `bin/check`, commit, push. Then on each Mac: `git pull && bin/apply`.
- **Lost device:** run `bin/revoke <name>` and push. Then `git pull && bin/apply` on **every** Mac,
  and remove that device's SSH key from `authorized_keys`.
- **Re-key a machine** (e.g. after reinstalling it): delete `/opt/homebrew/etc/nebula/host.key` there and run
  `bin/apply`. Then on the mini run `bin/sign <name> --force`.
- **Expiry:** the CA lasts 5 years. Phones and iPads last 1 year (re-sign with `--force`).
  A device cert can't outlive the CA.

## Security model

- **The open port:** only UDP 443 on the mini. Nebula ignores anyone without a certificate from
  this CA, so port scans see nothing. Keep Nebula updated (`brew upgrade nebula && bin/apply`).
- **Private keys:** every device creates its own key, and keys never leave the device. The CA key
  lives only on the mini (passphrase-encrypted). You can keep it offline and bring it back
  only to sign.
- **SSH:** key-only, and only from mesh addresses (`AllowUsers user@10.213.0.0/16`).
- **Screen Sharing:** gives full desktop control. Turn it on only on machines where you need it,
  and leave the "VNC viewers may control screen with password" option off.
- **Repo access:** anyone who can push to this repo can change what `bin/apply` installs, so keep it private.

## Limits

- Nebula only uses UDP. Networks that block **all** UDP (some strict corporate networks)
  can't join. UDP 443 gets through most networks because QUIC/HTTP3 uses it.
- A machine has to be awake to be reachable. A laptop with the lid closed on battery is not.
- If home internet or power is down, new connections fail, but connections already open keep working.
  To fix that, add a cheap VPS as a second lighthouse/relay.
- `bin/apply` supports macOS only (Homebrew). Linux nodes can be added later.

## Check

`bin/check` runs offline without sudo. It runs shellcheck, renders every `hosts.conf` machine's
config against a throwaway CA, checks it with `nebula -test`, and validates the sshd drop-in.
