# Briard

**Self-hosting. Just the fun part.** Briard runs self-hosted apps, like Home Assistant and
Mealie, on the Windows or Linux PC you already have. One button installs an app, one click
undoes a change, and your data stays home.

What it does, and why: **[briard.io](https://briard.io)**.

> **Status: alpha.** It has run on only a few machines outside our own. Please
> [tell us what broke](https://github.com/briardhq/briard/issues).

## Install

**Windows 10 or 11:** download the installer from [briard.io](https://briard.io).

**Linux** (any recent distribution, 64-bit):

```sh
curl -fsSL https://get.briard.io/install.sh | sudo sh
```

When it finishes, it prints a one-time link to the machine's Briard page. Open it and pick
your apps. If the link has expired, `sudo briard open` prints a new one.

## More

- [The briard command](https://briard.io/docs/cli/): what to run when something looks off.
  `briard doctor`'s output belongs in every bug report.
- [ARCHITECTURE.md](ARCHITECTURE.md): how it is built and why, and what it sends where.
- [CONTRIBUTING.md](CONTRIBUTING.md): building from source, testing, and proposing a change.

Found a security bug? Email **security@briard.io** rather than opening a public issue.

Apache-2.0, see [LICENSE](LICENSE). The third-party software included in a release, and where
to find its source, is listed in [THIRD-PARTY.md](THIRD-PARTY.md).
