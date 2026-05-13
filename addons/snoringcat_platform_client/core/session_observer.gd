class_name PlatformSessionObserver
extends Node
## Passive lifecycle bus for network-session events.
##
## The game-side session coordinator (Hop'n'Bop's
## `GameSessionManager`) owns the actual provider switching,
## rollback-netcode integration, and disconnect handling. It
## forwards each lifecycle event into the matching signal on this
## node. Addon subsystems (and future second-game code) subscribe
## here instead of taking a hard dependency on the game-side class.
##
## Why a passive observer rather than a real coordinator: the
## coordinator reaches into `Netcode.*` (rollback-netcode autoload),
## the matchmaker's `SessionProvider` extension, and per-game UI
## state. Lifting the coordinator into the addon would force the
## addon to depend on rollback-netcode and the game's UI surfaces.
## A passive bus keeps the dependency arrow pointing addon→Platform
## and game→Platform, never addon→game.
##
## Forwarded surface:
##   - `session_started(player_ids)` — local player IDs assigned by
##     the server after a successful match-ready hand-off.
##   - `match_ready` — server side: every expected player has
##     validated and connected.
##   - `connection_lost(reason_name, is_expected)` — client lost the
##     connection to the server (clean match end or otherwise).
##   - `matchmaking_failed(reason)` — pre-connect matchmaking
##     failure (auth invalid, timeout, allocation error, ...).
##   - `matchmaking_progress(phase, elapsed_sec, estimated_total_sec)`
##     — progress ticks from the matchmaker; consumed by the
##     loading-screen UI.


signal session_started(player_ids: Array[int])
signal match_ready()
signal connection_lost(reason_name: String, is_expected: bool)
signal matchmaking_failed(reason: String)
signal matchmaking_progress(
	phase: String,
	elapsed_sec: float,
	estimated_total_sec: float,
)
