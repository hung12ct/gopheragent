# Rollback runbook

## Standard service rollback

    kubectl rollout undo deployment/<service> -n production
    kubectl rollout status deployment/<service> -n production --timeout=5m

Verify the pods report the previous image tag before declaring recovery.

## When the deploy included a migration

Do not roll back the deployment first. Order matters:

1. Scale writers to zero.
2. Run the down migration.
3. Roll back the deployment.
4. Scale writers back up.

Skipping step 1 leaves in-flight writes against a schema that is being removed.
