# fleet

A self-hosted [Nebula](https://github.com/slackhq/nebula) network for my machines and phones.
You register each device once by pasting two short blocks of text. After that, every machine
gets a fixed mesh IP that works on any network, and admin devices can SSH or screen-share into
it from anywhere. No third-party servers are involved.

```
                         internet: only UDP 443 is forwarded, to the mini
                                         │
 laptop   (admin)   ─┐                   ▼
 office   (managed) ─┼── Nebula ──► Mac mini 10.213.0.1   lighthouse + relay + CA
 phone    (admin)   ─┘   tunnels   ├─ ~/.fleet/fleet.db  registry (SQLite): devices, IPs, certs, revocations
                                   └─ fleet serve :7443  API reachable ONLY inside the mesh
 every Mac: `fleet sync` (launchd, every 5 min) pulls its config, cert and revocations from
            the API, updates this repo's code with `git pull`, and restarts nebula only if something changed
```

- **No IPs to choose and no device files.** The registry assigns the next free mesh IP, signs
  the certificate, and keeps the revocation list.
- **Private keys never leave their device.** A join request carries only the public key, and the response
  only certificates and config, so you can pass both by clipboard, AirDrop or iMessage.
- **Admin from anywhere.** Admin devices approve, revoke and list devices through the
  mesh. Signing needs the CA passphrase, which you type on the admin device every time and which is never stored.
- **Access policy:** `admin` devices can reach every machine on TCP 22 (SSH) and 5900 (Screen Sharing).
  `managed` machines accept connections from admins but start none. Every device can reach the
  API to sync. Ping works between all devices. Everything else is dropped.

## Commands

| Where | Command | What it does |
|---|---|---|
| Mac mini, once | `fleet init --host H [--lan-ip IP]` | Creates the CA, the registry and the Mac mini's cert, and starts nebula, `fleet serve` and `fleet sync` |
| New Mac | `fleet join NAME` | Prints a join request, waits for you to paste the approval, then installs everything |
| Admin device | `fleet approve [--group admin\|managed]` | Turns a pasted join request into the response to paste back |
| Admin device | `fleet approve-mobile NAME` | Signs a public key from the Mobile Nebula app |
| Admin device | `fleet ls` | Lists devices: IP, group, last check-in, cert expiry, status |
| Admin device | `fleet revoke NAME` | Cuts a device off everywhere within about 5 minutes |
| Admin device | `fleet set NAME --group G` | Moves a device to another group (its cert is re-signed) |
| Any Mac | `fleet status` | Shows the services, whether the lighthouse is reachable, and this machine's IP |

## Set up the Mac mini (lighthouse)

**Before you start**
1. **Router:** reserve a LAN IP for the Mac mini and forward **UDP 443** to it. Check for CGNAT:
   the router's WAN IP must equal `curl ifconfig.me`. If it doesn't, port forwarding can't work
   (the fallback is to host the lighthouse on a VPS).
2. **DDNS:** set up a hostname for your home connection, e.g. `home.example.com`, through router DDNS or the Cloudflare API.
3. **Energy settings:** turn on "Prevent automatic sleeping" and "Start up automatically after a power failure".
4. **Tools:** Homebrew, `git`, and `python3` 3.9 or newer (Homebrew's, or the Xcode Command Line Tools version).

**Install the code with a read-only deploy key.** `fleet sync` pulls code updates without
anyone at the keyboard, so give each machine a key with no passphrase that can only read this repo:
```bash
ssh-keygen -t ed25519 -f ~/.ssh/fleet_deploy -N '' -C "fleet@$(scutil --get LocalHostName)"
cat ~/.ssh/fleet_deploy.pub
#   → GitHub: bdatdo0601/fleet → Settings → Deploy keys → Add (leave "write access" OFF)
#     or, from any machine with gh: gh repo deploy-key add fleet_deploy.pub -R bdatdo0601/fleet -t mini
GIT_SSH_COMMAND="ssh -i ~/.ssh/fleet_deploy -o IdentitiesOnly=yes" \
  git clone git@github.com:bdatdo0601/fleet.git ~/fleet
git -C ~/fleet config core.sshCommand "ssh -i ~/.ssh/fleet_deploy -o IdentitiesOnly=yes"
ln -s ~/fleet/bin/fleet /opt/homebrew/bin/fleet
```

**Create the fleet**
```bash
fleet init --host home.example.com --lan-ip 192.168.1.50
```
It asks you to set a CA passphrase (store it in your password manager), then asks for sudo, then prints the **CA id**.
Back up `~/.fleet/ca.key` and `~/.fleet/fleet.db`. To confirm it worked:
```bash
fleet status        # nebula, sync and serve loaded; lighthouse reachable as 10.213.0.1
fleet ls            # one device: mini
```

## Register your laptop (first admin, at home)

```bash
# laptop: same deploy-key clone as above (or use your existing checkout), then
fleet join laptop                    # prints a FLEET JOIN REQUEST block and waits
# mini:
fleet approve --group admin          # paste the request; check the request id matches; type the passphrase
# laptop: paste the FLEET JOIN RESPONSE block. Check the CA id matches what init printed.
```
To move the blocks between the two Macs, use Universal Clipboard (same Apple ID), AirDrop, or Messages.
Now the laptop can run `fleet ls`, `fleet approve` and the rest from any network, and you never need to log in to the Mac mini again.

## Register more machines later, from anywhere

On the new Mac, run `fleet join office`. Send the request block to your laptop, which can be on any
network, run `fleet approve` there (add `--group admin` only if this machine should control other machines),
and paste the response back. The approval goes through the mesh to the Mac mini, and the new machine joins immediately.

To make a machine reachable over SSH: turn on Remote Login in System Settings, run `bin/ssh-harden`
(key-only logins, accepted only from the mesh), and add your admin devices' SSH public keys to `~/.ssh/authorized_keys`.
For screen sharing, turn on Screen Sharing and limit it to your user.

## Register a phone or iPad

1. Install **Mobile Nebula** (by Defined Networking) and add a site. Have it generate a key, then copy the **public key**.
2. On an admin Mac, run `fleet approve-mobile phone` and paste the key. It prints a certificate, the CA and the lighthouse address.
3. In the app, paste the certificate and CA. Add a host: `10.213.0.1`, mark it as a **lighthouse**, with address `home.example.com:443`.
4. Add an SSH client (Blink, Termius), create a key, and authorize it on the Macs you'll manage.

Phone and iPad certs last 1 year. To renew one, run `fleet set phone --group admin` and paste the new certificate into the app.

## Day to day

- `ssh datdo@10.213.0.2`, or connect a VNC client to `10.213.0.2:5900`.
- Logging in to gcloud from your phone: `source ~/fleet/shell/gauth.zsh` in the target Mac's `~/.zshrc`, then
  run `gauth` over SSH and follow the URL-and-code prompts.
- **Lost a device:** run `fleet revoke NAME`, and remove its SSH key from `authorized_keys` on the machines it could reach.
- **Reinstalled a Mac:** run `fleet join NAME --force` on it, then `fleet approve --replace` (it keeps its IP).
- **Logs:**
  - `/var/log/fleet-sync.log` (every Mac)
  - `~/.fleet/serve.log` (Mac mini)
  - `/opt/homebrew/var/log/nebula.log`

## Security model

- **Internet exposure:** only UDP 443 on the Mac mini. Nebula ignores any packet without a certificate
  from this CA, so port scans see nothing. SSH and the API are never port-forwarded.
- **API:** listens on port 7443 but answers only loopback and mesh addresses. Nebula ties a mesh
  source IP to the sender's certificate, so the API knows which device is calling. Sync is allowed for
  every active device. Approve, revoke, set and list are allowed only for `admin` devices and the Mac mini.
- **CA key:** stored on the Mac mini only, and encrypted. The passphrase travels inside the encrypted
  tunnel for each approval and is used only in memory.
- **Paste channels:** each request shows a **request id** on both machines, and each response shows the **CA id**. Compare them
  to detect a block that was swapped in transit.
- **Code updates:** `fleet sync` runs as root and runs this repo's code. Anyone who can push
  to `bdatdo0601/fleet`, or any process running as your user on a machine, can therefore get root on it. Keep the
  repo private and your GitHub account protected with 2FA. Deploy keys are read-only.

## Limits

- Nebula only uses UDP. Networks that block all UDP can't join. UDP 443 gets through most networks.
- A machine has to be awake to be reachable.
- If your home connection is down, devices can't open new connections or sync, but existing connections keep working.
- macOS only for now (Homebrew + launchd).

## Development

```bash
python3 -m unittest discover -s tests      # uses the real nebula/nebula-cert; ~1 min
uvx ruff check . && shellcheck bin/fleet bin/ssh-harden
```
