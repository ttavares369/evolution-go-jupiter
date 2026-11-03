# Authenticated webhook connection hotfix

`Dockerfile.webhook-hotfix` adds the WhatsApp `Client outdated (405)` correction to the existing `jupiter2-webhook` image recipe. It preserves the original webhook patch, `WHATSAPP_WEBHOOK_SECRET`, exact destination restrictions, redirect protection, API endpoints, runtime version and PR #154 lifecycle fixes. No application code, application configuration, instance data or database schema is changed by this build.

The separate workflow publishes a new AMD64/ARM64 image tag, `0.7.2-jupiter2-webhook-wa405-<12-character-commit>`, without overwriting existing image tags or deploying any server. It runs both the webhook tests and the handshake-version regression test. This is the appropriate hotfix variant for Evolution servers that deliver authenticated callbacks to LembrAI.

Replace only the Evolution API service's image reference with the published tag or multiarch digest. Keep its environment, webhook secret, PostgreSQL and volume mount references exactly as configured. Validate connection and callbacks after deploying the Evolution API. No LembrAI deployment is required for this version propagation fix.

Do not change or rename database volumes during this update. Top-level volume declarations must continue to match the mount sources used by the services; use the actual current Coolify configuration to resolve any mismatch before deploying.
