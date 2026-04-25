# Backend

AWS SAM stack: `snoringcat-platform-backend`. Single shared
deployment for all Snoring Cat games. Per-game scoping via
`game_id` JWT claim and request paths.

## Structure

```
src/
  handlers/       # API Gateway entry points (Lambda functions)
  services/       # Business logic, DynamoDB access
  models/         # Pydantic request/response models
  utils/          # Cross-cutting helpers (auth, game_id_resolver)
tests/            # pytest + moto
scripts/
  deploy.ps1
  migrate-from-hopnbop.py
template.yaml     # SAM template
```

## Running tests

```bash
cd backend
pip install -r tests/requirements.txt
python -m pytest tests/ -v
```

## Deploying

TODO Phase 1.

## Migration

TODO Phase 1: `migrate-from-hopnbop.py` converts existing
`hopnbop-*` tables into the new schema with `game_id="hopnbop"`
backfilled on existing rows. Idempotent. Supports `--dry-run`.
