# Changelog

Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versioning: [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Backend and client SDK ship together as one shared semver release.
URL versioning (`/v1/`, `/v2/`) keeps old game clients working
across major bumps.

## [Unreleased]

### Added
- Initial repo structure: `backend/`, `addons/snoringcat_platform_client/`,
  `docs/`.
- Backend and client SDK CI workflow scaffolding.
- Target API spec (`docs/api-spec.md`).
- Per-game configuration reference (`docs/per-game-config.md`).
- Client SDK integration guide skeleton (`docs/client-sdk-guide.md`).
- `Platform` autoload skeleton (subsystems wired up in Phase 2).
