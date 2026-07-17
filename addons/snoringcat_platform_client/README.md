# Snoring Cat Platform Client

Godot 4.7 addon. Wraps the Snoring Cat platform backend HTTP API
and ships reusable UI building blocks (auth screen, friends panel,
party UI, side-panel framework, screen state machine, focus
navigator, settings persistence).

## Consuming this addon

Add as a git submodule from your game repo:

```bash
git submodule add \
    https://github.com/SnoringCatGames/snoringcat-platform \
    third_party/snoringcat-platform
ln -s ../third_party/snoringcat-platform/addons/snoringcat_platform_client \
    addons/snoringcat_platform_client
```

Then in your game's bootstrap (`_ready` of your `Main` autoload or
similar):

```gdscript
Platform.initialize({
    "game_id": "yourgame",
    "api_base_url": "https://api.snoringcat.games/v1",
    "sdk_version": "0.1.0",
})
```

## API surface

Game code reaches subsystems through the `Platform` autoload only:

| Property              | Purpose                                    |
|-----------------------|--------------------------------------------|
| `Platform.auth`       | Sign-in, sign-out, token refresh           |
| `Platform.account`    | Profile, link/unlink identity, delete      |
| `Platform.friends`    | List, add by code, accept/decline          |
| `Platform.party`      | Create, invite, leave                      |
| `Platform.presence`   | Set rich presence, read friend presence    |
| `Platform.settings`   | Local + cloud sync, scoped global/game     |
| `Platform.matchmaking`| Start, poll, cancel                        |
| `Platform.screens`    | Built-in screens (auth, consent, etc.)     |

## Compliance test suite

`test/compliance/` contains a GUT test directory that consuming
games run from their own CI. When a game submodule-bumps the
platform and a behavior breaks, the game's CI catches it on the
bump PR before merging.

To run from a consuming game:

```bash
godot --headless --path . -s addons/gut/gut_cmdln.gd \
    -gdir=res://addons/snoringcat_platform_client/test/compliance \
    -gexit
```

## Status

Skeleton. Subsystems get filled in during Phase 2 of the platform
extraction (see plan in `~/.claude/plans/`).
