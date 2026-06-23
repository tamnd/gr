---
title: "Installation"
description: "Install gr from Go, Homebrew, Scoop, a release archive, a Linux package, or the container image."
weight: 20
---

## Go module

To use gr as a library in your Go program:

```bash
go get github.com/tamnd/gr@latest
```

To install the `gr` CLI via Go:

```bash
go install github.com/tamnd/gr/cmd/gr@latest
```

Requires Go 1.21 or later.
gr is pure Go with no cgo, so it cross-compiles to any target `GOOS/GOARCH` pair.

## Homebrew (macOS and Linux)

```bash
brew install tamnd/tap/gr
```

This installs the latest release from the [tamnd/homebrew-tap](https://github.com/tamnd/homebrew-tap) tap.
Upgrades later with `brew upgrade gr`.

## Scoop (Windows)

```powershell
scoop bucket add tamnd https://github.com/tamnd/scoop-bucket
scoop install gr
```

## Linux package repositories

Signed `apt` (Debian/Ubuntu) and `dnf` (Fedora/RHEL) repositories are available.

**Debian/Ubuntu:**

```bash
curl -fsSL https://tamnd.github.io/linux-repo/gpg.key | sudo gpg --dearmor -o /usr/share/keyrings/tamnd.gpg
echo "deb [signed-by=/usr/share/keyrings/tamnd.gpg] https://tamnd.github.io/linux-repo stable main" \
  | sudo tee /etc/apt/sources.list.d/tamnd.list
sudo apt update && sudo apt install gr
```

**Fedora/RHEL:**

```bash
sudo dnf config-manager --add-repo https://tamnd.github.io/linux-repo/tamnd.repo
sudo dnf install gr
```

**Alpine:**

```bash
apk add gr --repository https://tamnd.github.io/linux-repo/alpine/
```

## Release archives

Download pre-built binaries for your platform from the [releases page](https://github.com/tamnd/gr/releases).

Available targets: `linux_amd64`, `linux_arm64`, `linux_armv7`, `linux_386`, `darwin_amd64`, `darwin_arm64`, `windows_amd64`, `windows_arm64`, `freebsd_amd64`, `freebsd_arm64`.

Each release includes `.tar.gz` archives (Linux, macOS, FreeBSD), `.zip` archives (Windows), and `.deb`, `.rpm`, `.apk` packages for Linux.

All archives contain a `checksums.txt` file and cosign signatures for verification.

**Verify a release:**

```bash
cosign verify-blob gr_1.0.0_linux_amd64.tar.gz \
  --signature gr_1.0.0_linux_amd64.tar.gz.sig \
  --certificate gr_1.0.0_linux_amd64.tar.gz.pem \
  --certificate-identity-regexp 'https://github.com/tamnd/gr' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'
```

## Container image

```bash
docker run --rm -v "$PWD:/data" ghcr.io/tamnd/gr shell /data/graph.gr
```

The image is based on Alpine and contains only the `gr` binary.
It is multi-platform: `linux/amd64`, `linux/arm64`, `linux/arm/v7`.

## Build from source

```bash
git clone https://github.com/tamnd/gr
cd gr
go build -o gr ./cmd/gr
```

Run the full test suite:

```bash
go test ./...
CGO_ENABLED=1 go test -race ./...
```

Next: [quick start](/getting-started/quick-start/).
