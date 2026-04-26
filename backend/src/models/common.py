"""Shared response shapes used across multiple endpoints."""

from __future__ import annotations

from pydantic import BaseModel, ConfigDict, Field


# All models share strict config: extra fields raise. This makes
# the Pydantic models a true contract (a typo'd field would
# silently no-op without strict mode).
class _BaseModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class ErrorResponse(_BaseModel):
    """Standard error envelope for non-2xx responses.

    Every handler returns this shape on validation failures,
    auth failures, and unexpected exceptions. The OpenAPI doc
    references this as the default response for 4xx/5xx.
    """

    status: str = Field(
        default="error",
        description=(
            "Always the literal string 'error' for failures."
        ),
    )
    error_code: str = Field(
        description=(
            "Stable machine-readable code clients can branch "
            "on (e.g. MISSING_PARAMS, INVALID_REFRESH)."
        ),
    )
    message: str = Field(
        description=(
            "Human-readable error message. Not stable; do not "
            "branch on this."
        ),
    )


class SuccessResponse(_BaseModel):
    """Generic 'no body' success used by void endpoints."""

    status: str = Field(default="success")
