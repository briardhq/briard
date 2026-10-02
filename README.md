# Briard

**The easiest way to run popular self-hosted apps (like Home Assistant and Immich) on a machine you already own.** You just install Briard, it manages your apps handling all the sysadmin work for you.

> **Status: alpha.** Built and tested, including in a long-running fault soak, but it has run
> on only a few machines outside our own. Please [tell us what broke](https://github.com/briardhq/briard/issues).

## Features

- **One command to install.** No image to flash, no VM to create, no tutorial to follow. It
  runs on the Linux you already have.
- **Zero config.** No accounts, no passwords, Home Assistant is ready to run.
- **Updates that undo themselves.** Before an app updates, its data is snapshotted. The new
  version must come back healthy; if it does not, the app and its data go back to the last good
  version on their own.
- **History and undo.** Snapshots are taken around every change and every hour. No matter what you break, open Briard and undo it in one click.
- **Keeps itself current.** Briard and its VM update themselves every night from a signed
  release channel, and every download is verified before use. An update that fails its health
  check reverts.
- **Built-in reverse proxy.** Every app gets its own name on your network
  (`briard-<name>-<app>.local`), and the proxy routes it. There are no ports to remember, and the
  name keeps working if the address changes.
- **HTTPS with a real certificate.** Optionally, claim a free `<name>.briard.casa` name. Briard
  gets a certificate for it and renews it on its own, and the private key never leaves your
  machine.
- **Encrypted storage.** App data lives on an encrypted (LUKS2) volume.
- **Looks after itself.** Briard brings back a VM that stops answering and gives the VM more
  memory when the apps need it. It warns when something needs you: updates that stopped, a
  machine running out of memory, a clock out of sync. `sudo briard alerts` lists the warnings;
  set `NOTIFY_URL` in `/opt/briard/config.env` to an [ntfy](https://ntfy.sh) topic and restart
  `briard-agent` to get them on your phone.
- **Works offline.** Your home runs locally and keeps running when the internet is down.

### Coming soon

- **Failover.** Add a second machine, and your apps replicate to it and move over within
  seconds if the first one fails.
- **Windows.** The same one-command install on a wired Windows desktop.
- **More self-hosted apps.** Immich, Syncthing, and more.

## Install

On the machine that will run your apps:

```sh
curl -fsSL https://get.briard.io/install.sh | sudo sh
```

When the installer finishes, it prints a one-time link to the machine's Briard page:

> `http://briard-<name>.local/?code=…`

Open it and press **Set up Home Assistant**. It takes a minute or two, and then **Open Home
Assistant** takes you into it, already logged in. Mosquitto (MQTT) is available from the same
page.

The link works once, within ten minutes. Run `sudo briard open` to print a new one.

## When something looks off

Run `sudo briard doctor` to check the machine, `sudo briard alerts` to see what it has warned
about, and `sudo briard logs` to read its logs (include these in a bug report).
`sudo briard help` lists every command.

## More

- [ARCHITECTURE.md](ARCHITECTURE.md): how it is built and why, and what it sends where.
- [CONTRIBUTING.md](CONTRIBUTING.md): building, testing, and proposing a change.

Found a security bug? Email **security@briard.io** rather than opening a public issue.

Apache-2.0, see [LICENSE](LICENSE). The third-party software included in a release, and where to
find its source, is listed in [THIRD-PARTY.md](THIRD-PARTY.md).
