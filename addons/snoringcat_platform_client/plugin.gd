@tool
extends EditorPlugin


const _AUTOLOAD_NAME := "Platform"
const _AUTOLOAD_PATH := \
		"res://addons/snoringcat_platform_client/core/platform.gd"


func _enter_tree() -> void:
	add_autoload_singleton(_AUTOLOAD_NAME, _AUTOLOAD_PATH)


func _exit_tree() -> void:
	remove_autoload_singleton(_AUTOLOAD_NAME)
