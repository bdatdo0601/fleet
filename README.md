# fleet

A self-hosted [Nebula](https://github.com/slackhq/nebula) network for your Macs and phones, run from
an always-on Mac at home. You register each device once by pasting two short blocks of text. After that,
every device keeps the same mesh IP on any network, and your admin devices can SSH or screen-share
into it from anywhere. No third-party servers are involved.

```
                         internet: only UDP 443 is forwarded, to the lighthouse
                                         │
 laptop   (admin)   ─┐                   ▼
 office   (managed) ─┼── Nebula ──► home Mac (lighthouse) 10.213.0.1   relay + CA
 phone    (admin)   ─┘   tunnels   ├─ ~/.fleet/fleet.db  registry (SQLite): devices, IPs, certs, revocations
                                   └─ fleet serve :7443  API reachable only from inside the mesh
 every Mac: `fleet sync` (launchd, every 5 min) pulls its config, cert and revocations from the
            API, updates this repo's code with `git pull`, and restarts nebula only if something changed
```

- **No IPs to choose and no device files.** The registry assigns the next free IP in `10.213.0.0/16`,
  signs the certificate, and keeps the revocation list.
- **Private keys never leave their device.** Join requests and responses contain only public keys,
  certificates and config. That makes it safe to pass them by clipboard, AirDrop or Messages.
- **Admin from anywhere.** Admin devices approve, revoke and list devices through the mesh. Signing
  needs the CA passphrase, which you type each time; it is never stored.
- **Central policy.** Firewall rules and revocations live on the lighthouse, and every Mac picks up
  changes within about 5 minutes.
- **Stdlib only.** Python 3.9+ and the `nebula` / `nebula-cert` binaries from Homebrew. Nothing else to install.

## Contents

[Commands](#commands) · [Setup](#setup) · [Adding devices](#adding-devices) ·
[Groups and access](#groups-and-access) · [Day to day](#day-to-day) · [Security model](#security-model) ·
[FAQ](#faq) · [Development](#development)

## Commands

| Where | Command | What it does |
|---|---|---|
| Lighthouse, once | `fleet init --host H [--route53-zone Z] [--lan-ip IP]` | Creates the CA, the registry and the lighthouse's cert, and starts nebula, the API, sync and (optionally) DNS updates |
| Lighthouse | `fleet ddns setup\|status\|policy` | Keeps the public DNS name pointed at home (Route 53) |
| New Mac | `fleet join NAME` | Prints a join request, waits for you to paste the approval, then installs everything |
| Admin device | `fleet approve [--group admin\|managed] [--replace]` | Turns a pasted join request into the response to paste back |
| Admin device | `fleet approve-mobile NAME` | Signs a public key from the Mobile Nebula app |
| Admin device | `fleet ls` | Lists devices: IP, group, last check-in, cert expiry, status |
| Admin device | `fleet revoke NAME` | Cuts a device off everywhere within about 5 minutes |
| Admin device | `fleet set NAME --group G` | Moves a device to another group (re-signs its cert, also used to renew one) |
| Any Mac | `fleet status` | Shows the services, whether the lighthouse is reachable, and this device's IP |

## Setup

### 1. Prerequisites (lighthouse)

- **An always-on Mac** with Homebrew, `git` and `python3` 3.9+. Homebrew's Python works, and so does the one in the Xcode Command Line Tools.
  In Energy settings, turn on "Prevent automatic sleeping" and "Start up automatically after a power failure".
- **Router:** reserve the Mac's LAN IP and forward **UDP 443** to it. Check for CGNAT: the router's WAN IP must equal
  `curl ifconfig.me`. If they differ, port forwarding can't work, and the lighthouse has to run somewhere with a public IP.
- **A DNS name** that points at your home connection, e.g. `lighthouse.example.com`. If your DNS is on Route 53, `fleet`
  can keep it updated itself ([step 3](#3-dns-updates-route-53-optional)). Otherwise, use your router's DDNS.

### 2. Install

```bash
git clone https://github.com/bdatdo0601/fleet.git ~/fleet
ln -s ~/fleet/bin/fleet /opt/homebrew/bin/fleet
```
Do the same on every Mac that will join. `fleet sync` updates each clone with `git pull --ff-only` every 5 minutes.

### 3. DNS updates (Route 53, optional)

`fleet ddns` checks every 5 minutes whether your public IP still matches what the zone's own nameservers answer.
Only when it differs does it update the A record (TTL 60). After your home IP changes, devices reconnect within
about 6 minutes, and connections that are already open keep working.

1. **Find the hosted zone ID.** In the console: Route 53 → Hosted zones → your domain → *Hosted zone details*. Or with the CLI:
   `aws route53 list-hosted-zones-by-name --dns-name example.com --query 'HostedZones[0].Id' --output text`.
2. **Create an IAM user** `fleet-ddns`: no console access and no managed policies. Give it one **inline policy**, which
   `fleet ddns policy --zone-id Z… --name lighthouse.example.com` prints for you. The policy allows only an
   UPSERT of that single A record, so a stolen key can't touch the rest of the zone.
3. **Create an access key** for that user on its *Security credentials* tab (use case: "Application running outside AWS").
4. **Configure the lighthouse** with the key:
   ```bash
   brew install awscli
   aws configure --profile fleet-ddns     # paste the key; region us-east-1
   ```

### 4. Create the fleet

```bash
fleet init --host lighthouse.example.com --route53-zone Z… --lan-ip 192.168.1.50
```
`init` first checks the AWS setup (when `--route53-zone` is given), so nothing is created if it's missing.
Then it:
1. asks you to set a **CA passphrase**, typed twice. Store it in your password manager; you'll need it for every approval.
2. asks for sudo.
3. creates the CA and registry in `~/.fleet`.
4. installs and starts the services.
5. points DNS at your public IP.
6. prints the **CA id**.

Back up `~/.fleet`. To confirm it worked:
```bash
fleet status        # nebula, sync, serve and ddns loaded; lighthouse reachable as 10.213.0.1
fleet ddns status   # public IP == DNS answer
fleet ls            # one device: mini
```

## Adding devices

### Your first admin device (at home)

```bash
# laptop
fleet join laptop                    # prints a FLEET JOIN REQUEST block and waits
# lighthouse
fleet approve --group admin          # paste it; check the request id matches; type the passphrase
# laptop: paste the FLEET JOIN RESPONSE block. Check the CA id matches what init printed.
```
To move the blocks between the two Macs, use Universal Clipboard, AirDrop or Messages. From now on the laptop can
run `fleet approve`, `fleet ls` and the rest from any network. You don't need to log in to the lighthouse again.

### More Macs, from anywhere

On the new Mac, run `fleet join office` and send the request block to an admin device. There, run `fleet approve`,
adding `--group admin` only if this Mac should be able to manage others, and paste the response back.
The approval travels through the mesh to the lighthouse, and the new Mac joins right away.

### Phones and tablets

1. In **Mobile Nebula** (Defined Networking), add a site, have it generate a key, and copy the **public key**.
2. On an admin Mac, run `fleet approve-mobile phone` and paste the key. It prints a certificate, the CA and the lighthouse address.
3. Paste the certificate and CA into the app. Add host `10.213.0.1`, marked as a **lighthouse**, with address `lighthouse.example.com:443`.
4. In an SSH client (Blink, Termius), create a key and authorize it on the Macs you want to reach.

### Making a Mac reachable

- **SSH:** turn on System Settings → General → Sharing → **Remote Login**, then run `~/fleet/bin/ssh-harden`.
  It allows key-only logins, accepted only from mesh addresses. Add your admin devices' public keys to
  `~/.ssh/authorized_keys`.
- **Screen sharing:** turn on **Screen Sharing**, limited to your user, and connect a VNC client to `<mesh IP>:5900`.
  Leave "VNC viewers may control screen with password" off.

## Groups and access

Each certificate carries one group. The CA signs it into the certificate, so a device can't claim a different one.

| From ↓ / To → | lighthouse | admin Mac | managed Mac | phone |
|---|---|---|---|---|
| **admin** (laptop, phone) | SSH, VNC, ping, API | SSH, VNC, ping | SSH, VNC, ping | ping¹ |
| **managed** | ping, API (sync only) | ping | ping | ping¹ |
| **lighthouse** | — | ping | ping | ping¹ |

¹ Phones use the Mobile Nebula app's own firewall settings.

- Through the API, **admin** devices (and the lighthouse itself) can approve, revoke, list and regroup devices.
  **managed** devices can only pull their own config.
- Every other port is dropped. Traffic relayed through the lighthouse is checked by the same rules at the destination.
- To change the rules, edit [`templates/`](templates/) and push. Every Mac applies the new rules on its next sync.
- To move a device to another group, run `fleet set NAME --group admin|managed`.

## Day to day

- `ssh you@10.213.0.2`, or connect a VNC client to `10.213.0.2:5900`. Run `fleet ls` to see every device's IP.
- **gcloud login from a phone:** add `source ~/fleet/shell/gauth.zsh` to the target Mac's `~/.zshrc`. Then, over SSH,
  run `gauth`: open the URL on the phone, sign in, and paste the code back. Running `gauth` again reattaches if the connection drops.
- **Lost a device:** run `fleet revoke NAME`, and remove its SSH key from `authorized_keys` wherever you added it.
- **Reinstalled a Mac:** run `fleet join NAME --force` on it, then `fleet approve --replace`. It keeps its IP.
- **Logs:** `/var/log/fleet-sync.log` (every Mac), `~/.fleet/serve.log` and `~/.fleet/ddns.log` (lighthouse),
  `/opt/homebrew/var/log/nebula.log`.

## Security model

- **Internet exposure:** only UDP 443 on the lighthouse. Nebula drops any packet that isn't from a
  certificate this CA signed, so port scans see nothing. SSH and the API are never port-forwarded.
- **API:** answers only loopback and mesh addresses. Nebula ties a mesh source IP to the sender's
  certificate, which is how the API knows who is calling. A LAN host can't spoof a mesh address,
  because the replies go into the tunnel and never reach it.
- **CA key:** lives only on the lighthouse, encrypted. The passphrase travels inside the tunnel for
  each approval and is used only in memory.
- **Paste channels:** both ends show a **request id** and a **CA id**. Compare them to detect a block
  that was swapped in transit.
- **Code updates run as root.** `fleet sync` runs this repo's code as root, so anyone who can push
  to the repo (or modify a machine's clone) can get root on every Mac that follows it. If you fork
  this repo, protect your account with 2FA, and review anything before merging.

## FAQ

**Do I have to re-register a device when it changes networks?**
No. A device's identity is its certificate, not its network. It keeps its mesh IP at home, at the office, on hotel
Wi-Fi or on a hotspot. After a switch, it reports its new address to the lighthouse within about a minute.

**Do certificates renew automatically?**
No. Signing needs the CA passphrase, and it is never stored.
- **Phone and tablet certs last 1 year.** Renew with `fleet set phone --group admin`, then paste the new cert into the app.
  The old cert is revoked immediately, so do both steps together.
- **Mac certs** last until the CA expires.
- **The CA lasts 5 years.** After that, re-run setup and re-join every device.

**Can devices in different groups reach each other?**
Admin devices can reach everything on SSH and VNC. Managed machines can only ping and sync. See [Groups and access](#groups-and-access).

**Does my traffic go through the lighthouse?**
Only when two devices can't connect directly, for example a phone on cellular reaching a laptop behind a strict NAT.
Even then it stays end-to-end encrypted, so the lighthouse can't read it. Relayed traffic is limited by your home upload speed.

**What if a network blocks UDP?**
Nebula only uses UDP. Port 443 gets through most networks, because QUIC/HTTP3 uses it, but a network that blocks all UDP
can't join. The device reconnects on its own once it's back on a normal network.

**What happens when the lighthouse or my home internet is down?**
Existing connections keep working. New connections, syncs and approvals wait until the lighthouse is back.
A Mac has to be awake to be reachable.

**How do I manage the lighthouse without exposing SSH to the internet?**
Approve one admin device while you're at home. After that, `fleet` commands run from that device over the mesh. If you
need a shell on the lighthouse, SSH to `10.213.0.1` from an admin device after running `ssh-harden` there.

**I mistyped the CA passphrase.**
`fleet approve` asks again, up to 3 times. To test the passphrase on the lighthouse without changing anything, sign a
throwaway key: `nebula-cert sign -ca-key ~/.fleet/ca.key -ca-crt ~/.fleet/ca.crt -name probe -networks 10.213.255.254/16 -in-pub <a .pub from nebula-cert keygen> -out-crt /tmp/probe.crt`.

**I lost the CA passphrase. How do I start over?**
It can't be recovered. On the lighthouse:
1. Stop the services: `for s in serve sync ddns; do sudo launchctl bootout system/io.github.bdatdo0601.fleet.$s; done`.
2. Move the old state aside: `mv ~/.fleet ~/.fleet.old`.
3. Run `fleet init` again.

Then re-join every device. Macs use `fleet join NAME --force`, which reuses any pending key.

**How many devices can it handle?**
The `/16` network has room for about 65,000. The lighthouse's own load is tiny. The real limit is relayed traffic
using your home upload, which doesn't matter for SSH but can slow screen sharing.

**Linux or Windows?**
Not yet. `fleet join` / `sync` assume macOS (Homebrew and launchd). Phones work through Mobile Nebula.

## Development

```bash
python3 -m unittest discover -s tests      # uses the real nebula/nebula-cert binaries; ~1 min
uvx ruff check . && shellcheck bin/fleet bin/ssh-harden
```
