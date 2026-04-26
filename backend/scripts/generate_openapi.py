#!/usr/bin/env python3
"""Generate openapi.json from the Pydantic models in src/models/.

Walks the ENDPOINTS table in src/models/__init__.py and emits a
standard OpenAPI 3.0.3 document. Used by the CI workflow's
oasdiff step to detect breaking schema changes between the
committed baseline and a PR branch.

Usage:

    # Print to stdout (default; CI uses this).
    python scripts/generate_openapi.py

    # Write to a file (handy for updating the baseline):
    python scripts/generate_openapi.py --out openapi.json

    # Pretty-print with 2-space indent (default; pass --compact
    # for the minified form CI compares).
    python scripts/generate_openapi.py --compact

The output references each Pydantic model under
`#/components/schemas/<ClassName>`. Pydantic 2's
`model_json_schema()` produces the per-model JSON schema; this
script wires them together into the OpenAPI envelope.

Schema conventions:
- Every endpoint declares ErrorResponse as the default 4xx/5xx
  response.
- 2xx response is the response_model (or SuccessResponse when
  the model is None).
- Auth-required endpoints declare a bearerAuth security scheme.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from typing import Any

# Make src/ importable so models/* resolves.
sys.path.insert(
    0,
    os.path.join(
        os.path.dirname(__file__), "..", "src"
    ),
)

from models import (  # noqa: E402
    ENDPOINTS,
    EndpointSpec,
    common,
)


# OpenAPI references all schemas via $ref into components/schemas.
# Pydantic 2 emits "$defs" in its schema; we rewrite to "components".
def _rewrite_refs(obj: Any) -> Any:
    """Replace Pydantic '#/$defs/X' refs with OpenAPI
    '#/components/schemas/X' refs in place."""
    if isinstance(obj, dict):
        if "$ref" in obj and isinstance(obj["$ref"], str):
            obj["$ref"] = obj["$ref"].replace(
                "#/$defs/", "#/components/schemas/"
            )
        return {
            k: _rewrite_refs(v) for k, v in obj.items()
        }
    if isinstance(obj, list):
        return [_rewrite_refs(v) for v in obj]
    return obj


def _collect_schemas(
    endpoints: list[EndpointSpec],
) -> dict[str, dict]:
    """Walk every model referenced by every endpoint and produce
    a flat {ClassName: schema} map for the components section.

    Pydantic returns each model's full JSON schema with nested
    types under "$defs"; we hoist those into the top-level
    components and rewrite refs.
    """
    schemas: dict[str, dict] = {}

    def _add_model(model_cls):
        if model_cls is None:
            return
        full = model_cls.model_json_schema(
            ref_template="#/components/schemas/{model}"
        )
        # The top-level schema is the model itself.
        defs = full.pop("$defs", {})
        schemas[model_cls.__name__] = _rewrite_refs(full)
        for name, sub in defs.items():
            schemas[name] = _rewrite_refs(sub)

    # Always include ErrorResponse in components since every
    # endpoint references it as the default error response.
    _add_model(common.ErrorResponse)
    _add_model(common.SuccessResponse)
    for ep in endpoints:
        _add_model(ep.request_model)
        _add_model(ep.response_model)

    return schemas


def _build_paths(
    endpoints: list[EndpointSpec],
) -> dict[str, dict]:
    """Build the paths section keyed by URL path."""
    paths: dict[str, dict] = {}
    for ep in endpoints:
        path_item = paths.setdefault(ep.path, {})
        op: dict[str, Any] = {
            "summary": ep.summary,
            "tags": list(ep.tags),
            "responses": {
                "200": _make_response_ref(
                    ep.response_model
                    if ep.response_model is not None
                    else common.SuccessResponse,
                ),
                "default": {
                    "description": (
                        "Error envelope used by all "
                        "non-2xx responses."
                    ),
                    "content": {
                        "application/json": {
                            "schema": {
                                "$ref": (
                                    "#/components/schemas/"
                                    "ErrorResponse"
                                )
                            }
                        }
                    },
                },
            },
        }
        if ep.request_model is not None:
            op["requestBody"] = {
                "required": True,
                "content": {
                    "application/json": {
                        "schema": {
                            "$ref": (
                                "#/components/schemas/"
                                f"{ep.request_model.__name__}"
                            )
                        }
                    }
                },
            }
        if ep.auth_required:
            op["security"] = [{"bearerAuth": []}]
        path_item[ep.method] = op
    return paths


def _make_response_ref(model_cls) -> dict:
    """Build a 200-response object referencing a Pydantic model."""
    return {
        "description": "Successful response.",
        "content": {
            "application/json": {
                "schema": {
                    "$ref": (
                        "#/components/schemas/"
                        f"{model_cls.__name__}"
                    )
                }
            }
        },
    }


def build_openapi() -> dict:
    """Produce the full OpenAPI 3.0.3 document."""
    return {
        "openapi": "3.0.3",
        "info": {
            "title": "Snoring Cat Platform API",
            "version": "v1",
            "description": (
                "Shared backend for Snoring Cat multiplayer "
                "titles. URL-versioned (/v1/, /v2/, ...) so "
                "older game clients keep working across "
                "platform releases. See docs/api-spec.md for "
                "the human-readable overview."
            ),
        },
        "servers": [
            {
                "url": "https://api.snoringcat.games",
                "description": (
                    "Production. The actual API Gateway URL "
                    "is also reachable; this hostname is the "
                    "future canonical alias."
                ),
            }
        ],
        "tags": [
            {
                "name": "auth",
                "description": "Sign-in and account linking.",
            },
            {
                "name": "friends",
                "description": (
                    "Global cross-game friends graph."
                ),
            },
            {
                "name": "presence",
                "description": (
                    "Cross-game presence (which game am I in)."
                ),
            },
            {
                "name": "misc",
                "description": (
                    "Versioning, telemetry, ad-hoc utilities."
                ),
            },
        ],
        "components": {
            "securitySchemes": {
                "bearerAuth": {
                    "type": "http",
                    "scheme": "bearer",
                    "bearerFormat": "JWT",
                    "description": (
                        "JWT issued by /v1/auth/*. Includes "
                        "player_id and game_id claims."
                    ),
                }
            },
            "schemas": _collect_schemas(ENDPOINTS),
        },
        "paths": _build_paths(ENDPOINTS),
    }


def main():
    parser = argparse.ArgumentParser(
        description=(
            "Generate openapi.json from src/models/."
        )
    )
    parser.add_argument(
        "--out",
        default=None,
        help=(
            "Output file. Default: write to stdout (CI uses "
            "this for the diff step)."
        ),
    )
    parser.add_argument(
        "--compact",
        action="store_true",
        help=(
            "Emit minified JSON. Default is 2-space indent."
        ),
    )
    args = parser.parse_args()

    doc = build_openapi()
    if args.compact:
        text = json.dumps(
            doc, sort_keys=True, separators=(",", ":")
        )
    else:
        text = json.dumps(doc, sort_keys=True, indent=2)

    if args.out:
        with open(args.out, "w", encoding="utf-8") as f:
            f.write(text)
            f.write("\n")
        print(
            f"Wrote {len(text)} bytes to {args.out}",
            file=sys.stderr,
        )
    else:
        print(text)


if __name__ == "__main__":
    main()
