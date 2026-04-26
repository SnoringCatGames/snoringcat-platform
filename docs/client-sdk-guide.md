# Client SDK guide

How to integrate `snoringcat-platform` into a new Godot game.

> **Status:** partial. The backend is fully production-ready;
> the client SDK addon covers the foundation (auth token store,
> generic HTTP client, screen + transition base classes, toast
> overlay, input device manager, view utils, GUT compliance
> tests) but does not yet bundle the API clients (friends,
> party, backend) or the UI panels (friends, party, account,
> settings, auth, consent). Those still live in the consuming
> game (currently Hop 'n Bop) and will migrate over time.

## Repo layout

The platform is composed of three repos that pin into a game
via git submodules:

| Repo | Path inside game | What's in it |
|------|------------------|--------------|
| [`snoringcat-platform`](https://github.com/SnoringCatGames/snoringcat-platform) | `third_party/snoringcat-platform` | Backend SAM stack + client SDK addon |
| [`godot-rollback-netcode`](https://github.com/SnoringCatGames/godot-rollback-netcode) | `addons/rollback_netcode` | Rollback netcode addon |
| [`godot-gamelift-session-manager`](https://github.com/SnoringCatGames/godot-gamelift-session-manager) | `addons/gamelift_session_manager` | GameLift session manager + patched WebRTC GDExtension |

## Setup steps

1. **Add the submodules**:

   ```bash
   git submodule add \
       https://github.com/SnoringCatGames/snoringcat-platform \
       third_party/snoringcat-platform
   git submodule add \
       https://github.com/SnoringCatGames/godot-rollback-netcode \
       addons/rollback_netcode
   git submodule add \
       https://github.com/SnoringCatGames/godot-gamelift-session-manager \
       addons/gamelift_session_manager
   ```

2. **Bridge the platform addon** into your game's `addons/`
   directory. Godot reads its addons from `res://addons/`, but
   the platform addon lives under `third_party/`. Two options:

   - **xcopy bridge (recommended on Windows)**: copy the addon
     into `addons/snoringcat_platform_client/` after each
     submodule update. See `hopnbop_private/scripts/setup-platform-addon.ps1`
     for a reference implementation.
     This avoids a Godot 4.6 parser-cache bug that bites
     directory junctions on Windows.

   - **Symlink (Mac/Linux)**: `ln -s` the addon directory in.
     Faster but untested on Windows under Godot 4.6.

3. **Register your game**: see
   [per-game-config.md](per-game-config.md) for the steps to
   insert your game's row in the `snoringcat-games` config
   table. Until that exists the backend rejects sign-in attempts
   with your `game_id`.

4. **Add `Platform` to the autoload list** in
   `project.godot`:

   ```
   Platform="*res://addons/snoringcat_platform_client/core/platform.gd"
   ```

5. **(For now) Use the addon's primitives directly**. The
   addon currently exposes:

   - `core/api_client.gd` — generic HTTP client (load via
     runtime `load()`, NOT preload — see the file's docstring).
   - `core/auth_token_store.gd` — JWT + refresh token storage.
   - `core/platform.gd` — autoload entry point.
   - `input/*` — device manager, focus navigator, generic
     input poller.
   - `ui/overlays/toast_overlay.gd` — toast notifications.
   - `ui/screens/screen.gd` + `screen_transition.gd` — screen
     stack + tile-shader transition.
   - `util/*` — pixel viewport manager, font fallback config.

   API clients (friends, party, backend) and UI panels are
   still in the consuming game's `src/` tree. They'll move
   into the addon as they get parameterized. Until then, your
   game can reference Hop 'n Bop's implementations as a
   blueprint:
   - `hopnbop_private/src/core/{friends,party,backend}_api_client.gd`
   - `hopnbop_private/src/ui/settings_panel/{friends,party,account}_panel.gd`

## Compliance tests

Run the platform compliance suite from your game's CI to catch
contract breaks when bumping the platform submodule:

```yaml
- run: |
    powershell -File scripts/setup-platform-addon.ps1
    godot --headless --path . -s addons/gut/gut_cmdln.gd \
      -gdir=res://addons/snoringcat_platform_client/test/compliance \
      -gexit
```

The suite probes live (default) — every CI run hits the actual
backend. Set `PLATFORM_COMPLIANCE_MODE=mock` to skip live
calls (mock-mode interception is not yet implemented; tests
mark themselves `pending`). Override the API URL via
`PLATFORM_API_URL=...` to point at a staging stack.

Current coverage: `/v1/version`, `/v1/auth/anon`, and a
route-existence probe across every authenticated endpoint.

## Cross-game presence (for games shipped on the platform)

The backend issues JWTs with a `game_id` claim and uses it to:

- Route matchmaking, sessions, and fleet ops to the correct
  GameLift fleet (looked up from `snoringcat-games`).
- Stamp parties with the leader's `game_id` and reject
  `/v1/party/invite` to friends in a different game.
- Track per-friend `game_id` on `/v1/presence/heartbeat`
  responses so the friends UI can show "in <other game>"
  badges. Hop 'n Bop's `friends_panel.gd` is the reference
  implementation.

## Open work

- Move API clients into the addon (`Platform.friends`,
  `Platform.party`, `Platform.backend`).
- Move UI panels into the addon (themeable; consuming game
  passes a `Theme` resource).
- `Platform.initialize({game_id, api_base_url, ...})` to
  replace the current ad-hoc init.
- Mock-mode HTTP interception for the compliance suite.
- Theming guide for auth/consent/friends screens.
