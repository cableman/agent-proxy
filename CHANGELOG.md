# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `task package` builds a universal `Agent Proxy.app` (Apple Silicon and Intel)
  with a valid ad hoc signature and a correct minimum macOS version (13.0).
- `task run:app` packages and opens the macOS app bundle.

### Changed

- The fyne CLI is pinned in `tools/go.mod` and run through `go tool`, so
  `task package` no longer depends on `PATH` or an unpinned `@latest` install.
- The app icon is rendered at 1024 px so it stays sharp on Retina displays.

### Fixed

- The app ID is set in code, so preferences and the file dialog's last folder
  work no matter where the binary is started from.
- macOS builds through Task no longer show the duplicate `-lobjc` linker
  warning, and `task deps` and `task icon` no longer print spurious warnings.
- `task deps` on Debian/Ubuntu installs the Wayland headers that current Fyne
  needs to build.
