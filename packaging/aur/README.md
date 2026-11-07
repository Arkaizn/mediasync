# AUR packages

| Package | Builds | Users update with |
|---------|--------|-------------------|
| **mediasync** (`mediasync/`) | the latest release tag | plain `yay` / `paru` |
| **mediasync-git** (`mediasync-git/`) | the latest commit of `main` | `yay -Syu --devel` (yay only checks git sources with `--devel`) |

Both install the same files and conflict with each other. After an update
the running service restarts itself on the new version (it watches its own
executable; see `cmd/mediasyncd/selfupdate.go`), so no manual
`systemctl --user restart` is needed.

## Releasing (mediasync)

1. Move the `[Unreleased]` entries in `CHANGELOG.md` under the new version.
2. Tag and push:
   ```sh
   git tag v0.2.0 && git push origin v0.2.0
   ```
   (or create a release with a new tag on GitHub).
3. The workflow `.github/workflows/aur-release.yml` sets `pkgver`, the
   tarball checksum and `.SRCINFO`, and pushes to the AUR. Every system
   gets the update with its next `yay`.

**One-time setup:** the workflow needs the repository secret
`AUR_SSH_PRIVATE_KEY` (*Settings → Secrets and variables → Actions*): a
private key whose public key is in your AUR account (*My Account → SSH
Public Key*). Use a key made only for this:
```sh
ssh-keygen -t ed25519 -N '' -C mediasync-aur-ci -f ~/.ssh/aur_ci
cat ~/.ssh/aur_ci.pub    # add to the AUR account (one key per line)
cat ~/.ssh/aur_ci        # paste as the secret, then: rm ~/.ssh/aur_ci
```
The first run creates the AUR package `mediasync`. The workflow only talks
to `aur.archlinux.org` if its host key matches the pinned fingerprint.

## mediasync-git

Only needs a new upload when the PKGBUILD changes (dependencies, build or
install steps): edit it, bump `pkgrel`, then
```sh
git clone https://aur.archlinux.org/mediasync-git.git && cd mediasync-git
git remote set-url --push origin ssh://aur@aur.archlinux.org/mediasync-git.git
cp ../packaging/aur/mediasync-git/PKGBUILD .
makepkg --printsrcinfo > .SRCINFO      # always regenerate before pushing
git commit -am "…" && git push
```
SSH access to the AUR: add your public key in the AUR account and
```
# ~/.ssh/config
Host aur.archlinux.org
  User aur
  IdentityFile ~/.ssh/id_ed25519_personal
  IdentitiesOnly yes
```

## Installing

```sh
yay -S mediasync          # or mediasync-git
systemctl --user daemon-reload
systemctl --user enable --now mediasyncd
```

Switching from `mediasync-git` to `mediasync`: `yay -S mediasync` and
confirm removing `mediasync-git`. Old `mediasync-git` builds (before
pkgrel 2) left read-only files in yay's cache; remove them once with
`chmod -R u+w ~/.cache/yay/mediasync-git && rm -rf ~/.cache/yay/mediasync-git`.

The packages install `/usr/bin/mediasyncd`, `/usr/bin/mediasyncctl`, the
user unit `/usr/lib/systemd/user/mediasyncd.service`, and an example config
in `/usr/share/doc/mediasync/config.toml` (copy it to
`~/.config/mediasync/config.toml`). If you installed with `make install`
before, remove that copy first (`make uninstall`) so the user unit in
`~/.config/systemd/user` does not shadow the packaged one.
