"""Tests for scripts/generate_openapi.py.

Verifies the generated OpenAPI doc is well-formed, references
every Pydantic model registered in ENDPOINTS, and stays in sync
with the committed openapi.json baseline.

The "baseline drift" test catches the common failure mode where
someone adds an endpoint or changes a model but forgets to
re-run the generator.
"""

from __future__ import annotations

import importlib.util
import json
import os
import sys

import pytest


_SCRIPT_PATH = os.path.join(
    os.path.dirname(__file__),
    "..",
    "scripts",
    "generate_openapi.py",
)
_BASELINE_PATH = os.path.join(
    os.path.dirname(__file__),
    "..",
    "openapi.json",
)


def _load_generator():
    spec = importlib.util.spec_from_file_location(
        "generate_openapi", _SCRIPT_PATH
    )
    mod = importlib.util.module_from_spec(spec)
    sys.modules["generate_openapi"] = mod
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture
def generator():
    return _load_generator()


@pytest.fixture
def doc(generator):
    return generator.build_openapi()


class TestStructure:
    def test_openapi_version(self, doc):
        assert doc["openapi"] == "3.0.3"

    def test_has_info(self, doc):
        assert "title" in doc["info"]
        assert doc["info"]["version"] == "v1"

    def test_has_security_scheme(self, doc):
        schemes = doc["components"]["securitySchemes"]
        assert "bearerAuth" in schemes
        assert schemes["bearerAuth"]["scheme"] == "bearer"

    def test_paths_nonempty(self, doc):
        assert len(doc["paths"]) > 0


class TestEndpointCoverage:
    """Every ENDPOINTS row must appear as a path/method op."""

    def test_every_endpoint_appears(self, doc):
        from models import ENDPOINTS

        for ep in ENDPOINTS:
            assert (
                ep.path in doc["paths"]
            ), f"missing path {ep.path}"
            assert (
                ep.method in doc["paths"][ep.path]
            ), (
                f"missing method {ep.method} on "
                f"{ep.path}"
            )

    def test_request_body_for_post_with_model(self, doc):
        from models import ENDPOINTS

        for ep in ENDPOINTS:
            op = doc["paths"][ep.path][ep.method]
            if ep.request_model is not None:
                assert "requestBody" in op
                schema_ref = op["requestBody"]["content"][
                    "application/json"
                ]["schema"]["$ref"]
                assert (
                    ep.request_model.__name__ in schema_ref
                )
            else:
                # GET / DELETE / no-body POST: no requestBody.
                assert "requestBody" not in op

    def test_auth_endpoints_have_security(self, doc):
        from models import ENDPOINTS

        for ep in ENDPOINTS:
            op = doc["paths"][ep.path][ep.method]
            if ep.auth_required:
                assert op.get("security") == [
                    {"bearerAuth": []}
                ]
            else:
                assert "security" not in op

    def test_default_response_is_error(self, doc):
        for path, methods in doc["paths"].items():
            for method, op in methods.items():
                default = op["responses"]["default"]
                ref = default["content"][
                    "application/json"
                ]["schema"]["$ref"]
                assert ref.endswith("/ErrorResponse"), (
                    f"{method.upper()} {path} default "
                    f"response is not ErrorResponse"
                )


class TestSchemas:
    def test_error_response_in_schemas(self, doc):
        assert (
            "ErrorResponse"
            in doc["components"]["schemas"]
        )

    def test_every_referenced_schema_exists(self, doc):
        """No path may $ref a schema that's not in components."""
        components = set(
            doc["components"]["schemas"].keys()
        )

        def _check_refs(obj):
            if isinstance(obj, dict):
                if "$ref" in obj and isinstance(
                    obj["$ref"], str
                ):
                    ref = obj["$ref"]
                    if ref.startswith(
                        "#/components/schemas/"
                    ):
                        name = ref.split("/")[-1]
                        assert name in components, (
                            f"dangling $ref: {ref}"
                        )
                for v in obj.values():
                    _check_refs(v)
            elif isinstance(obj, list):
                for v in obj:
                    _check_refs(v)

        _check_refs(doc)

    def test_schemas_have_no_leftover_defs_refs(self, doc):
        """All Pydantic '$defs' refs were rewritten to OpenAPI."""
        text = json.dumps(doc)
        assert "#/$defs/" not in text


class TestBaselineDrift:
    """If src/models/* changed without regenerating openapi.json,
    this test fails. Re-run scripts/generate_openapi.py to fix."""

    def test_baseline_matches_current(self, generator):
        if not os.path.exists(_BASELINE_PATH):
            pytest.skip(
                "openapi.json baseline not present yet"
            )
        with open(_BASELINE_PATH) as f:
            baseline = json.load(f)
        current = generator.build_openapi()
        if baseline != current:
            pytest.fail(
                "openapi.json is out of sync with src/models/. "
                "Re-run: python scripts/generate_openapi.py "
                "--out openapi.json"
            )
