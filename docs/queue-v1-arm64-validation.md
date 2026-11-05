# Evolution Go ARM64 — final-2 / provisioning atual

Data: 2026-11-05. Cwd: `evolution-go-jupiter`. Execução isolada em `.work/arm-finalization/final-2`.

## Conclusão

**SIM: ao término da validação, as fontes que produziram a imagem final-2 correspondem exatamente ao fork atual.** Não se reutilizou o Dockerfile.userspace antigo. Foram congelados 39 arquivos de entrada do build a partir do fork atual e regenerada a mesma adaptação RUN exec + QEMU userspace. Esses 39 arquivos continuam byte a byte iguais ao fork. A árvore recomposta independentemente (upstream fixo + patches + overlay + testes + VERSION) coincide em **169 arquivos**, sem diferenças, com o stage build da imagem nova. O binário do stage coincide por SHA-256 com `/app/server` da imagem final.

A imagem final é ARM64, CGO_ENABLED=1, e executou server/HTTP/SQL/shutdown e codecs sob QEMU. Não é apenas prova de ELF. O workflow local editado para jupiter3 **não entrou sequer no contexto de build**, tampouco no rootfs final.

Não houve publicação, push, commit, staging, produção, conta/sessão WhatsApp real, licença real, DSN real, binfmt install, builder privilegiado ou Docker restart. No projeto, só houve escritas dentro deste scratch final-2. A tag/imagem e todos os arquivos de evidência da execução anterior foram preservados.

**Não é homologação nativa Oracle/Ampere nem aprovação de produção.**

## Imagem nova e hashes

| Item | Valor |
| --- | --- |
| Tag final local | `evolution-go-jupiter:arm-finalization-20261105-9320-final-2` |
| Image ID / OCI index local | `sha256:5de79a7633e7d463c61b9ce26c63c8b2a2e6d94f9aab249cefa752192ec8e876` |
| Manifest da imagem | `sha256:f2bb9f25e26cbc16503d44accf38c32c90ab468cae945139bbd0864fe94d8adb` |
| Config | `sha256:1af7a48b59673992fd8481520b7dc14aa84207ac559314856a2a5127967345bf` |
| Plataforma | `linux/arm64` |
| Size reportado | `274465381` bytes |
| Versão | `0.7.2-jupiter3-queue-v1` |
| SHA-256 `/app/server` | `89ad99f35410e8373f3d69a7c8aa2fe219315957c8f4b63077a6ece0f7bf60af` |
| SHA-256 Dockerfile atual/original no snapshot | `3feb2cce228f83b67faa2022b2f77924739d10c66a0c95607c3878fa9603f817` |
| SHA-256 Dockerfile.userspace regenerado | `2b9408ffbfbafb9920ad2fbd4e21dcbe9e9397af2476d4f99e42cdd7a8a1bd3c` |
| SHA-256 exclusive-instances.patch atual | `cf8ce370d3e54c3386e2dab0f878d0c7285c0c46a304e20724cac842ef8b7427` |
| SHA-256 manifesto independente dos 169 arquivos | `a00798b6df4ebf0a25b752d9d43af5ec978c35a45a29a709516299fea77cffc0` |
| SHA-256 workflow local observado, excluído do build | `bdaffe8f35ac6e1a2393dc8f2224c7b60bb7e699377118f63fb6a65f727681f2` |
| Stage build local auxiliar | `evolution-go-jupiter:arm-finalization-20261105-9320-final-2-tools` |

ENTRYPOINT original da imagem nova preservado: `/app/server`; WORKDIR `/app`, TZ America/Sao_Paulo, EXPOSE 8080. Nos smokes o entrypoint foi explicitamente substituído por QEMU externo montado readonly; não há QEMU persistido no rootfs final. Os três arquivos de manager/dist coincidem com o stage de build.

Buildinfo do server final: Go1.25.0, CGO_ENABLED=1, GOARCH=arm64, GOOS=linux, GOARM64=v8.0, ldflags `-X main.version=0.7.2-jupiter3-queue-v1`, vcs.revision `9337afc47e10b86cc896a6f432240e40fee95dd1`, vcs.modified=true. O commit é a base upstream, não um commit inventado do fork; os patches não foram commitados.

## Correspondência dos fontes — como foi provada

1. `fork-before.json` registra SHA-256 dos **46 arquivos do fork** fora de .git/.work, inclusive workflow, docs e README.
2. `context/` foi criado só com Dockerfile, .dockerignore, patches/, overlay/ e os dois testes explicitamente copiados pelo Dockerfile. **39 arquivos**, hashes em `build-inputs.json`; sem .github, docs, README, env, sessões ou cache. O build usou esse snapshot, não uma árvore mutável durante a compilação.
3. `Dockerfile.userspace` foi regenerado a partir do Dockerfile desse snapshot. A única adaptação é o mecanismo de execução de cada RUN: mount readonly do emulador + exec JSON `[qemu, /bin/sh, -c, comando-original]`. `transformation.json` registra hashes e os comandos originais. Nenhum comando de teste/build foi removido ou trocado; nenhuma alteração no Dockerfile do projeto.
4. Clone/patches conferidos pela própria receita, inclusive o hash novo acima. Commit upstream fixo: `9337afc47e10b86cc896a6f432240e40fee95dd1`.
5. Extração de `/build` e `/tmp/queue-v1-legacy-baseline` de containers **novos, nunca iniciados**, da nova imagem tools, usando entrypoint `/not-executed`.
6. `verify.py` executou Git archive da árvore upstream imutável, aplicou PR154 via mailsplit/apply, webhook, conferiu o baseline de **137 arquivos**; depois logs, overlay atual, queue wiring, exclusive atual, testes e VERSION. Git apply foi isolado com GIT_CEILING_DIRECTORIES no scratch, sem descobrir o repo pai e sem index/commits.
7. Comparação SHA-256 de **169 arquivos** entre a recomposição `reference/` e o stage build `source/`, excluindo apenas .git e o binário gerado server: **zero diferenças**, inclusive novo provisioning/admin, policy/activation e wiring em main.
8. Rootfs final exportado temporariamente: `/app/server` tem o mesmo SHA-256 do stage, VERSION correto, manager/dist idêntico, zero caminhos QEMU e zero caminhos app/.github. Export temporário removido.
9. Nova comparação dos hashes atuais do fork/snapshot: **zero diferenças nos 39 inputs; todos os 46 arquivos do fork sem alteração durante a tarefa**. Não se afirmou que docs/workflow precisam ser incluídos no runtime: eles foram preservados, mas não são fontes de runtime copiadas pela receita.

Evidência estruturada: `verification.json`, `build-inputs.json`, `composed-source-manifest.json`, `build-source-manifest.json`, `compose.log`, `server-buildinfo.log`, `image-inspect.log`.

## Build ARM64 final-2

- Host x86_64, builder default com driver docker, BuildKit v0.34.2, somente amd64 e variantes v2/v3. Nenhuma alteração no builder/host.
- Mesmo emulador da execução anterior, copiado para o novo scratch: buildkit-qemu-aarch64, QEMU 5.2.0 (`v4.0.0-14050-gbe25039802`); SHA registrado em transformation.json. Não houve nova instalação nem registro de kernel.
- **Build final: retorno 0, 630.28 s.** Log: `build-final-2.log`.
- **Stage tools: retorno 0, 20.25 s**, usando cache do mesmo build. Log: `build-tools.log`.
- `go test -mod=readonly ./...` e `go vet -mod=readonly ./...` realmente executados na árvore atual sob ARM64/QEMU, não apenas reaproveitados da árvore antiga; etapa de testes/vet levou 573.5 s.
- `CGO_ENABLED=1 GOTOOLCHAIN=local go build -mod=readonly -ldflags "-X main.version=0.7.2-jupiter3-queue-v1" -o server ./cmd/evolution-go`: retorno 0, 15.6 s.
- Testes PostgreSQL opt-in não têm DSN na etapa Dockerfile; não alegar zero skips no `go test ./...` do build. Eles foram reexecutados separadamente com fixture real, abaixo.
- Bases resolvidas: golang:1.25.0-alpine `sha256:f18a072054848d87a8077455f0ac8a25886f2397f88bfdd222d6fafbb5bba440`; alpine:3.19.1 `sha256:c5b1261d6d3e43071626931fc004f70149baeba2c8ec672bd4f27761f8e1ad6b`.

## Testes críticos do código novo com PG

Imagem tools **nova final-2**, Go linux/arm64, GOPROXY=off, GOTOOLCHAIN=local, módulos readonly, sem race detector nesta execução:

```sh
go test -json -mod=readonly -count=1 -parallel=1 -timeout=180s ./pkg/queuev1 ./pkg/queuepolicy ./pkg/instance/handler
go test -json -mod=readonly -count=3 -parallel=1 -timeout=180s -run ^TestProvisioning ./pkg/queuev1
```

- Primeira suíte: **334 testes/subtestes PASS, 0 FAIL, 0 SKIP**, três pacotes. Inclui parsers, validade, ledger/migração, política, IDOR e novo provisioning.
- Repetição crítica: **36 eventos PASS, 0 FAIL, 0 SKIP**: 12 testes/subtestes de provisioning em cada uma de três repetições. Há sobreposição com a suíte anterior; não somar como 370 testes únicos.
- `TestProvisioningRealAuthAdminAndContractBeforeSQL`: auth admin real e contrato antes do SQL; 401/428, UUID inválido, chave admin não substitui auth de instância.
- `TestProvisioningCommitAndGateProofBoundaries`: gate prévio, UUID ausente, COMMIT ambíguo, prova falsa/erro e sucesso.
- `TestProvisioningOptInAndAvailability`: opt-in desligado, dependências ausentes, gate indisponível, não anunciar capacidade inexistente.
- `TestProvisioningRealPostgresIdempotentStickyAndIsolated`: PG real, oito ativações concorrentes, timestamp sticky preservado, dados/token/estado da instância preservados, isolamento de outro UUID, restart sem perda de política, nenhum I/O de transporte, sem API de desativação.
- `TestProvisioningOptionalProductionWiring`: main liga o AuthAdmin existente ao opt-in queue.
- `TestProvisioningRegisterOptionalRealInitialization`: inicialização real com PG, rota registrada, capabilities anuncia provisioning em fixture de rotas, UUID ausente retorna 404 sem serviço WhatsApp.

Os testes HTTP isolados usam fixtures de instância/transporte, conforme o código do fork; não são conta/sessão/licença reais. Não se alteraram os testes nem fontes para fazê-los passar.

Logs: `arm-focused-pg.log`, `arm-provisioning-pg-repeat3.log`; contagens/nomes em `verification.json`.

## Smokes na imagem final nova

| Checagem | Resultado observado |
| --- | --- |
| `/app/server -h` | Executa e imprime help sob QEMU |
| Versão | VERSION e log startup `0.7.2-jupiter3-queue-v1`; buildinfo com ldflags correto |
| WebP bundled CGO | Roundtrip lossless 8x8, 54 bytes, pixel conferido |
| JPEG | Roundtrip 644 bytes; libjpeg.so.8 create/destroy com ABI80 |
| libwebp de sistema | dlopen/dlsym explícito de libwebp.so.7, decoder 0x10302 / 1.3.2, decode RGBA e pixel conferidos; não confunde com símbolo bundled |
| Testes de thumbnail do fonte novo no filesystem final | **6 PASS**, incluindo PDF real rasterizado por Poppler; sem skip |
| Runtime ferramentas | aarch64; FFmpeg 6.1.1; pdftoppm 23.10.0; libjpeg-turbo-3.0.1-r0, libwebp-1.3.2-r0 |
| GET /server/ok | 200, status ok |
| GET /manager/login | 200, HTML do manager |
| GET /license/status | 200, inactive |
| GET /queue/v1/capabilities | **503 LICENSE_REQUIRED** |
| POST /queue/v1/instances/00000000-0000-4000-8000-000000000001/exclusive | **503 LICENSE_REQUIRED**, mesmo com chave sintética e header queue-v1 |
| Log de rota do POST final | `route="/queue/v1/instances/:instanceId/exclusive"`, confirmando match da rota nova registrada |
| SQL após startup | instances=0; só UUID fictício de configuração na política; cinco tabelas queue_v1; runtime_configs apenas chave instance_id, sem licença persistida |
| SIGTERM | Shutdown limpo, `[SHUTDOWN] Server exited`, ExitCode=0, OOMKilled=false |

**Sem bypass de licença.** O POST de provisioning não teve sucesso no executável final e não se alegou o contrário. Sua semântica de sucesso/negação e PG foi exercitada pelas fixtures de rotas existentes na suíte ARM64; no runtime final só se comprovou o bloqueio de licença e o match da rota. Não houve criação de conta, sessão, envio ou uso de token real.

Logs: `final-runtime.log`, `final-codecs.log`, `final-thumbnail-pdf.log`, `http-smoke.log`, `startup-log.log`, `startup-db-evidence.log`, `app-exit.log`.

## Isolamento e preservação

- PG 16.15 **amd64** em container novo, usuário postgres, `--network none`, rootfs readonly, dados em tmpfs, capabilities removidas. Sem IP/gateway/port bindings.
- App ARM64, testes e cliente HTTP compartilham somente o namespace network none do PG; TCP apenas 127.0.0.1. Não se criou rede bridge nem se publicaram portas.
- DSN só de fixture: banco `evolution_queue_test_arm9320_final2`, usuário/senha sintéticos fixture/fixture-only; auth em fixture_auth. Nenhum envfile de projeto/produção foi montado.
- App com CONNECT_ON_STARTUP=false, DATABASE_SAVE_MESSAGES=false, MINIO_ENABLED=false, queue opt-in ligado e UUID fictício explícito, rootfs readonly, tmpfs /tmp, cap-drop ALL e no-new-privileges. QEMU montado readonly; sem volume de sessões.
- Chave sintética fixture-arm-smoke-not-a-license. Proxy HTTP(S) em loopback fechado :9, NO_PROXY loopback, namespace sem egress. A tentativa automática de licença não atingiu serviço externo nem ativou produto.
- Download de fontes/pacotes públicos ocorreu somente no build. Runtime não teve rede externa/WhatsApp.
- **254 arquivos de evidência anteriores sem mudança de bytes**; **16 tags preexistentes mantiveram os mesmos image IDs**. Inclui tag final antiga e imagem tools antiga; nenhuma foi retagueada/alterada.
- Containers final-2 removidos, inclusive os de extração/inspeção, PG e app. Restam duas novas tags locais, cache normal Docker e artefatos ignorados neste scratch.
- `binfmt_misc` permaneceu register/status/windows/windowsPE; builder continua amd64-only; nenhum restart ou instalação privilegiada.

## Ressalvas preservadas

1. Runtime Alpine/pacotes vieram da camada **CACHED** da execução anterior, inalterada (mesmos três primeiros diffIDs). Portanto o warning anterior `glib-2.78.6-r0.trigger: script exited with error 1` **não foi corrigido nem reavaliado numa instalação limpa**. Não há novo sucesso independente do apk nessa execução. Os codecs, FFmpeg, Poppler e thumbnails da imagem nova passaram. Não chamar isso de build sem ressalvas de instalação.
2. Startup novamente registrou três diagnósticos GORM query_error redigidos e falha NATS não fatal sem NATS configurado. Não impediram HTTP/SQL/shutdown; não foram corrigidos fora do escopo. Os logs de PG também contêm erros intencionais dos testes negativos de constraints/falhas de conexão; resultados de teste e runtime estão separados.
3. Nenhuma alegação de aprovação do comando loader `--list` que falhou na execução anterior. Nesta rodada a evidência é readelf/buildinfo + execução real do servidor e harnesses.
4. Não houve Oracle/Ampere nativo, licença ativa, conta/sessão real, envio WhatsApp, testes de reconexão real, race ARM64 ou benchmark nativo. A base Alpine antiga não foi atualizada/auditada nesta tarefa.

## Comandos efetivamente executados

A partir de `evolution-go-jupiter`, com paths internos já existentes nesta execução:

```sh
python3 .work/arm-finalization/final-2/prepare_build.py
```

Esse script congela e confere os inputs, gera Dockerfile.userspace e executa, com timeout:

```sh
docker buildx build --platform=linux/arm64 --progress=plain \
  --build-context armqemu=.work/arm-finalization/final-2/emulator \
  --load -f .work/arm-finalization/final-2/Dockerfile.userspace \
  -t evolution-go-jupiter:arm-finalization-20261105-9320-final-2 \
  .work/arm-finalization/final-2/context

docker buildx build --platform=linux/arm64 --progress=plain \
  --build-context armqemu=.work/arm-finalization/final-2/emulator \
  --load --target build -f .work/arm-finalization/final-2/Dockerfile.userspace \
  -t evolution-go-jupiter:arm-finalization-20261105-9320-final-2-tools \
  .work/arm-finalization/final-2/context

python3 .work/arm-finalization/final-2/smoke.py
python3 .work/arm-finalization/final-2/verify.py
```

Logs individuais têm argv exato, retorno e duração. Esses são registros da execução, não comandos para publicar. Prepare/verify são etapas one-shot que não sobrescrevem o snapshot/reference existente; para nova validação, usar outro scratch e outra tag. As operações docker create/cp/export/rm e recomposição estão nos scripts/transcritos locais.

## Evidência estruturada principal

- **`verification.json`**: correspondência fonte/binário, hashes, contagens e preservação.
- `fork-before.json`, `build-inputs.json`, `transformation.json`, `composed-source-manifest.json`, `build-source-manifest.json`.
- `build-final-2.log`, `build-tools.log`, `build-result.json`, `compose.log`.
- `arm-focused-pg.log`, `arm-provisioning-pg-repeat3.log`, `smoke-results.json`, `smoke-transcript.log`.
- `final-codecs.log`, `final-thumbnail-pdf.log`, `final-runtime.log`, `server-buildinfo.log`, `elf-evidence.log`.
- `http-smoke.log`, `startup-log.log`, `startup-db-evidence.log`, `app-network.log`, `fixture-network.log`, `app-exit.log`.
- `image-inspect.log`, `containers-final.log`, `buildx-final.log`, `binfmt-final.log`, `previous-evidence-before.json`, `images-before.txt`, `git-final.log`.
