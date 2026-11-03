# Standalone Evolution Go connection hotfix

Fixes `Client outdated (405)` by applying the discovered or configured WhatsApp Web version to the actual Whatsmeow handshake before constructing the client. Existing `jupiter1` lifecycle fixes (PR #154) are retained. The upstream source and PR #154 checksum remain pinned.

`Dockerfile.connection-hotfix` builds the generic `jupiter1` service without application-specific webhook patches. It does not require an application deployment. The existing Dockerfile, webhook patch, build workflow and published production tags are untouched.

The separate connection-hotfix workflow tests and builds natively for AMD64 and ARM64 and publishes `ghcr.io/ttavares369/evolution-go-jupiter:0.7.2-jupiter1-wa405-<12-character-commit>`. No server is automatically deployed. The runtime API version remains `0.7.2-jupiter1` for compatibility; the image tag identifies the hotfix.

For a service currently using `0.7.2-jupiter1`, replace only its Compose `image:` with the published hotfix tag and redeploy that Evolution service. Keep environment variables, PostgreSQL, volumes and instance records. Do not delete sessions or request logout. Validate connection logs and a controlled test before updating the other servers.

This standalone variant does not replace the webhook-authenticated `jupiter2-webhook` variant. Do not switch a service that relies on that webhook authentication to this generic image.

The regression test inspects the actual existing-session login payload after applying the discovered version and checks that invalid lookup results do not overwrite the working version. Live WhatsApp authentication still requires deployment and verification against the real service.
