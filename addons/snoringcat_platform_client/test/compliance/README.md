# Platform compliance test suite

GUT tests that consuming games run from their own CI on every
submodule bump of `snoringcat-platform`. When a platform change
breaks a game's expected behavior, the game's CI catches it on
the bump PR rather than at runtime in production.

## Running from a consuming game

```bash
godot --headless --path . -s addons/gut/gut_cmdln.gd \
    -gdir=res://addons/snoringcat_platform_client/test/compliance \
    -gexit
```

## Modes

The suite runs in one of two modes, controlled by an env var:

- **`PLATFORM_COMPLIANCE_MODE=mock`** (default) — `HTTPRequest` is
  intercepted; responses are fixtured per the OpenAPI schema. Fast,
  deterministic, suitable for game CI.
- **`PLATFORM_COMPLIANCE_MODE=live`** — Tests hit the real backend.
  Used by the hourly synthetic monitor in `snoringcat-platform`'s
  scheduled workflow.

## What gets tested

(Filled in incrementally as Phase 2 ports each subsystem.)

| Subsystem      | Test file                | Status   |
|----------------|--------------------------|----------|
| Auth           | `test_auth.gd`           | TODO P2  |
| Account        | `test_account.gd`        | TODO P2  |
| Friends        | `test_friends.gd`        | TODO P2  |
| Party          | `test_party.gd`          | TODO P2  |
| Presence       | `test_presence.gd`       | TODO P4  |
| Settings       | `test_settings.gd`       | TODO P2  |
| Matchmaking    | `test_matchmaking.gd`    | TODO P2  |
| Match loopback | `test_match_loopback.gd` | TODO P3  |
| API surface    | `test_api_surface.gd`    | TODO P2  |
