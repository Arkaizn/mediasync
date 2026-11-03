# AUR package

`mediasync-git/` contains the PKGBUILD for the AUR package **mediasync-git**,
which builds the latest commit of the default branch. Users update with
`yay -Syu --devel` (or `paru -Syu --devel`); the AUR package itself only
needs a new upload when the PKGBUILD changes (dependencies, build or
install steps).

## First upload

1. Add your SSH public key in your AUR account (*My Account → SSH Public Key*)
   and configure SSH for the AUR:
   ```
   # ~/.ssh/config
   Host aur.archlinux.org
     IdentityFile ~/.ssh/aur
     User aur
   ```
2. Check that the name is free: https://aur.archlinux.org/packages?K=mediasync
3. Test the build on Arch:
   ```sh
   cd packaging/aur/mediasync-git
   makepkg -si            # builds, runs the tests, installs
   namcap PKGBUILD *.pkg.tar.zst
   ```
   Set the `# Maintainer:` line to the name and (obfuscated) e-mail you want
   to publish.
4. Publish:
   ```sh
   git clone ssh://aur@aur.archlinux.org/mediasync-git.git aur-mediasync-git
   cp packaging/aur/mediasync-git/PKGBUILD aur-mediasync-git/
   cd aur-mediasync-git
   makepkg --printsrcinfo > .SRCINFO      # always regenerate before pushing
   git add PKGBUILD .SRCINFO
   git commit -m "Initial import: mediasync-git"
   git push
   ```

## Updating the AUR package

Only needed when the PKGBUILD changes: edit it, bump `pkgrel`, regenerate
`.SRCINFO`, commit and push to the AUR repository. A changed `pkgver` alone
does not need to be pushed (VCS packages compute it at build time).

## Installing after publishing

```sh
yay -S mediasync-git
systemctl --user daemon-reload
systemctl --user enable --now mediasyncd
```

The package installs `/usr/bin/mediasyncd`, `/usr/bin/mediasyncctl`, the user
unit `/usr/lib/systemd/user/mediasyncd.service`, and an example config in
`/usr/share/doc/mediasync/config.toml` (copy it to
`~/.config/mediasync/config.toml`). If you installed with `make install`
before, remove that copy first (`make uninstall`) so the user unit in
`~/.config/systemd/user` does not shadow the packaged one.
