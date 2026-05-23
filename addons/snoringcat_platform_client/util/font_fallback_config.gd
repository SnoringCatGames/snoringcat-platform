class_name PlatformFontFallbackConfig
extends RefCounted
## Configures font fallbacks for non-Latin scripts (CJK, Arabic,
## Devanagari, Thai). Loads Noto Sans variants and adds them as
## fallbacks on the supplied themes.
##
## Extracted from
## hopnbop/src/core/font_fallback_config.gd at tag
## pre-platform-extraction.
##
## Differences from the source:
## - Renamed FontFallbackConfig → PlatformFontFallbackConfig so
##   the original can stay in hopnbop until migrated.
## - Removed the hard-coded `G.settings.default_theme` and HUD
##   theme path. The consumer passes the themes to apply
##   fallbacks to (and the directory for the Noto fonts).
##
## Place these files in your fonts directory:
##   NotoSansSC-Regular.ttf  (Simplified Chinese)
##   NotoSansJP-Regular.ttf  (Japanese)
##   NotoSansKR-Regular.ttf  (Korean)
##   NotoSansArabic-Regular.ttf
##   NotoSansDevanagari-Regular.ttf  (Hindi)
##   NotoSansThai-Regular.ttf


const DEFAULT_FALLBACK_FILES := [
	"NotoSansSC-Regular.ttf",
	"NotoSansJP-Regular.ttf",
	"NotoSansKR-Regular.ttf",
	"NotoSansArabic-Regular.ttf",
	"NotoSansDevanagari-Regular.ttf",
	"NotoSansThai-Regular.ttf",
]


## Add Noto Sans fallbacks to every supplied theme.
##
## - `themes` — themes whose primary fonts should accept the
##   fallbacks. Skips null entries.
## - `noto_dir` — `res://`-rooted directory containing the
##   Noto Sans font files. Defaults to
##   `res://assets/fonts/noto/`.
## - `font_files` — file names to try inside `noto_dir`. Missing
##   files are silently skipped.
##
## Returns the number of fallback fonts that were actually loaded.
static func configure_fallbacks(
	themes: Array[Theme],
	noto_dir := "res://assets/fonts/noto/",
	font_files: Array = DEFAULT_FALLBACK_FILES,
) -> int:
	var fallback_fonts: Array[Font] = []
	for file_name in font_files:
		var path: String = noto_dir + file_name
		if not ResourceLoader.exists(path):
			continue
		var font: FontFile = load(path)
		if font != null:
			fallback_fonts.append(font)

	if fallback_fonts.is_empty():
		return 0

	for theme in themes:
		_add_fallbacks_to_theme(theme, fallback_fonts)

	return fallback_fonts.size()


static func _add_fallbacks_to_theme(
	theme: Theme,
	fallback_fonts: Array[Font],
) -> void:
	if theme == null:
		return

	var default_font := theme.default_font
	if default_font is FontFile:
		_add_fallbacks_to_font(
			default_font, fallback_fonts)

	# Also add to button font if different.
	if theme.has_font("font", "Button"):
		var button_font := theme.get_font(
			"font", "Button")
		if (button_font is FontFile
				and button_font != default_font):
			_add_fallbacks_to_font(
				button_font, fallback_fonts)


static func _add_fallbacks_to_font(
	font: FontFile,
	fallback_fonts: Array[Font],
) -> void:
	for fb in fallback_fonts:
		if fb not in font.fallbacks:
			font.fallbacks.append(fb)
