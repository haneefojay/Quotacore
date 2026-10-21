# Runbook: A leaked key

Part of [the runbooks](README.md). Every command assumes `QC_URL` is the API (for example
`http://127.0.0.1:8080`), `QC_METRICS` the metrics listener (`http://127.0.0.1:9090`) and
`QC_ADMIN_KEY` an admin key. The `quotacore` commands run the binary against the same environment
as the service.

## Trigger

Any suspected exposure of an API key.

## Impact

A runtime key can spend any tenant's allowance; an admin key can change any configuration.

## Check

1. Which key, by its prefix (the part before the dot), and its last use:

```sh
curl -sS -X GET -H "Authorization: Bearer $QC_ADMIN_KEY" "$QC_URL/v1/admin/keys"
```

2. For an admin key, every action it took:

```sh
curl -sS -X GET -H "Authorization: Bearer $QC_ADMIN_KEY" "$QC_URL/v1/admin/audit?limit=100"
```

## Decide

Any suspicion is enough: revoke first, investigate second.

## Act

Revoke it; the revocation is immediate:

```sh
curl -sS -X DELETE -H "Authorization: Bearer $QC_ADMIN_KEY" "$QC_URL/v1/admin/keys/$KEY_ID"
```

Issue a replacement with `POST /v1/admin/keys`, and redeploy the client that used it.

## Verify

A request with the revoked key answers `401 api_key_invalid`, and `quotacore_rate_limited_total` shows no further traffic under its prefix.

## Escalate

Signs of use you cannot explain in the audit log or the events: escalate as a security incident per [security-model.md section 9](../architecture/security-model.md#9-incident-response).

## Source

[Security-model.md section 9](../architecture/security-model.md#9-incident-response); DR-043.
