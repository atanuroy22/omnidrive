# OmniDrive

[![Go Version](https://img.shields.io/badge/go-1.24+-00ADD8?style=for-the-badge&logo=go)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg?style=for-the-badge)](LICENSE)
[![GitHub stars](https://img.shields.io/github/stars/atanuroy22/omnidrive?style=for-the-badge&logo=github)](https://github.com/atanuroy22/omnidrive/stargazers)

> All your cloud drives — Google Drive, OneDrive, Dropbox, pCloud, TeraBox, WebDAV, S3 — in one mobile web app, served by a **single 6.7 MB binary**.

No Node. No Docker. No database. No server to rent.

---

## The Concept

![Multi-cloud aggregator](docs/screenshots/multi-cloud-concept.png)

OmniDrive brings all your cloud storage into one unified interface. Connect Google Drive, OneDrive, Dropbox, and more — then browse, upload, and share files across all of them without switching apps.

---

## Screenshots

<table>
<tr>
<td align="center"><img src="docs/screenshots/all_drives.jpeg" width="200" alt="All drives" /><br/><sub>All connected drives</sub></td>
<td align="center"><img src="docs/screenshots/combined.jpeg" width="200" alt="Combined view" /><br/><sub>Unified file view</sub></td>
<td align="center"><img src="docs/screenshots/setting.jpeg" width="200" alt="Settings" /><br/><sub>Settings screen</sub></td>
</tr>
</table>
<p align="center"><sub>Android UI — same experience on Windows, Linux &amp; macOS</sub></p>

---

## Features

- **7 cloud providers** — Google Drive, OneDrive, Dropbox, pCloud, TeraBox, WebDAV, S3
- **Zero dependencies** — pure Go standard library
- **Encrypted state** — AES-256-GCM, PBKDF2-secured bundles
- **One-command device transfer** — pair two phones over Wi-Fi
- **Upload strategies** — most-free, least-used, round-robin, weighted, manual
- **Public file links** — one tap to share any file

---

## Download

Pre-built binaries for Windows EXE and Android APK:

| Platform | File |
|---|---|
| Android (APK) | `omnidrive-*.apk` |
| Windows | `omnidrive-windows-amd64.exe` |

👉 **[Download Latest Release](https://github.com/atanuroy22/omnidrive/releases/latest)**

---

## Getting Started

### Quick Install (Pre-built Binaries)

1. Go to [Latest Release](https://github.com/atanuroy22/omnidrive/releases/latest)
2. Download the binary for your platform
3. Run it:

**Linux/macOS:**
```bash
chmod +x omnidrive-linux-amd64
./omnidrive-linux-amd64
```

**Windows:**
```bash
omnidrive-windows-amd64.exe
```

**Android:**
- [Download](https://github.com/atanuroy22/omnidrive/releases/latest) the APK, then: install it.

### Build from Source

```bash
# All platforms
./build.sh

# Just Android
./build.sh android

# Just Windows
./build.sh windows-amd64
```

**Requirements:** Go 1.24+, JDK 17+, Android SDK (for APK).

---

## Supported Providers

| Provider | Auth | Setup |
|---|---|---|
| Google Drive | OAuth | [Google Cloud Console](https://console.cloud.google.com/apis/credentials) |
| OneDrive | OAuth | [Microsoft Entra](https://entra.microsoft.com) |
| Dropbox | OAuth | [Dropbox Console](https://www.dropbox.com/developers/apps) |
| pCloud | Email + password | None |
| TeraBox | Session cookie | None — **1 TB free** |
| WebDAV | URL + credentials | None — Nextcloud, Yandex, Koofr, NAS |
| S3 compatible | Access keys | None — AWS, R2, B2, MinIO, Wasabi |

---

## License

[MIT](LICENSE)
