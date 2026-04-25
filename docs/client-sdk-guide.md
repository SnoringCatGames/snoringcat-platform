# Client SDK guide

How to integrate `snoringcat-platform` into a new Godot game.

> **Status:** placeholder. Filled in during Phase 6 once the
> SDK API has stabilized.

## Quick start (target shape)

1. Add submodules to the game repo:

   ```bash
   git submodule add \
       https://github.com/SnoringCatGames/snoringcat-platform \
       third_party/snoringcat-platform
   git submodule add \
       https://github.com/SnoringCatGames/godot-rollback-netcode \
       third_party/godot-rollback-netcode
   git submodule add \
       https://github.com/SnoringCatGames/godot-gamelift-session-manager \
       third_party/godot-gamelift-session-manager
   ```

2. Symlink the addons (or copy them) into your game's `addons/`:

   ```bash
   ln -s ../third_party/snoringcat-platform/addons/snoringcat_platform_client \
       addons/snoringcat_platform_client
   # ... same for the other two ...
   ```

3. Register your game with the platform (one-time, populates the
   `games` config table):

   ```bash
   ./scripts/register-with-platform.ps1 \
       --game-id yourgame \
       --display-name "Your Game" \
       --fleet-id <gamelift-fleet-id> \
       --matchmaker <gamelift-matchmaker-name>
   ```

4. Initialize the SDK from your game's bootstrap:

   ```gdscript
   func _ready() -> void:
       Platform.initialize({
           "game_id": "yourgame",
           "api_base_url": "https://api.snoringcat.games/v1",
           "sdk_version": "0.1.0",
       })
   ```

5. Use the platform UI screens or build your own:

   ```gdscript
   Platform.screens.show("auth")
   ```

## Theming

(TODO Phase 6.)

## Compliance tests

Run the platform compliance suite from your game's CI:

```yaml
- run: godot --headless --path . -s addons/gut/gut_cmdln.gd \
    -gdir=res://addons/snoringcat_platform_client/test/compliance \
    -gexit
```
