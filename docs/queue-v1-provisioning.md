# Queue-v1 — adesão administrativa mínima (2026-11-05)

## Estado e escopo

Mudança **somente local no fork**, aditiva à finalização pós-6C. Sem alterações no SaaS, criação/conexão legada, credenciais, sessões, Whatsmeow ou workflow de publicação. A edição de tags `jupiter3` feita pelo usuário foi preservada byte a byte. Nenhum commit/staging/push/deploy, produção, mensagem real ou execução do servidor.

A allowlist estática do produtor **não é necessária para cada cadastro novo**. A autoridade de exclusividade é a policy persistida e consultada pelo gate. `EVOLUTION_QUEUE_V1_EXCLUSIVE_INSTANCE_IDS` continua disponível para adesão explícita de UUIDs, inclusive MASTER existente; não se tornou uma restrição da nova rota. Eventual allowlist restritiva do consumidor é decisão do SaaS, não alterada aqui.

## Endpoint e respostas exatas

Disponível no runtime normal somente com **`EVOLUTION_QUEUE_V1=true`**:

```http
POST /queue/v1/instances/:instanceId/exclusive
apikey: chave-global-administrativa-existente
X-Evolution-Queue-Contract: queue-v1
```

Não precisa de body ou Content-Type. A ação é sempre adesão: o UUID vem **somente do path**; o body não é interpretado como seleção, configuração ou desativação. Usa o **`AuthAdmin` legado real**, com `config.GlobalApiKey`; tokens de instância não autorizam nem mesmo o próprio UUID. Não há token novo. A chave global não passa a autorizar as rotas per-instância de envio/capabilities.

Sucesso inicial **e replay idempotente**: HTTP **200**, `Content-Type: application/json`, `Cache-Control: no-store`, `X-Evolution-Queue-Contract: queue-v1`:

```json
{
  "contract": "queue-v1",
  "instanceId": "10000000-0000-4000-8000-000000000001",
  "exclusiveInstance": true
}
```

O UUID acima é apenas exemplo. A ordem dos campos JSON não é parte do contrato.

| HTTP | JSON exato / significado |
| --- | --- |
| 428 | `{"error":"queue_contract_opt_in_required"}` — header ausente/divergente; verificado antes do auth |
| 401 | `{"error":"not authorized"}` — resposta original de AuthAdmin para global key ausente/incorreta ou token de instância |
| 400 | `{"error":"invalid_instance_id"}` — UUID não canônico, nulo ou inválido |
| 404 | `{"error":"instance_not_found"}` — UUID canônico sem linha no banco de instâncias deste servidor |
| 503 | `{"error":"queue_unavailable"}` — contrato/gate/DB indisponível, erro/timeout SQL, commit ambíguo ou falha da prova pós-commit |

Com opt-in desligado a rota **não é registrada**: 404 padrão Gin, não o JSON `instance_not_found`. Não existem DELETE/PUT/PATCH de desativação. Nenhuma resposta de erro contém `exclusiveInstance:true`, tokens, request body ou texto do driver.

### Fronteira de persistência/prova

1. AuthAdmin + header; UUID canônico não nulo.
2. Confirmar que o gate instalado está consultável. Não instalar um checker fictício como substituto.
3. Abrir transação curta no **mesmo pool PostgreSQL** usado pelo gate/instâncias.
4. Obter o mesmo `pg_advisory_xact_lock(719628120025100002)` da adesão por configuração.
5. `SELECT id FROM instances WHERE id=$1 FOR KEY SHARE`: comprovar existência e manter a linha contra remoção até commit. Não conectar/iniciar cliente, consultar WhatsApp, modificar linha ou rotacionar token.
6. Criar a tabela própria de policy se necessário e fazer `INSERT ... ON CONFLICT DO NOTHING` na mesma implementação compartilhada da configuração. O `activated_at` anterior não é reescrito.
7. **COMMIT confirmado** e então **consulta positiva de `queuepolicy.Exclusive(ctx, instanceId)`**. Só os dois juntos permitem HTTP 200 com true.

A operação HTTP tem orçamento de 5s; consultas do gate mantêm o limite próprio de 2s. Erro antes/durante commit nunca segue pelo caminho de sucesso, mesmo que o commit possa ter sido efetivado. Erro/false da prova depois de commit dá 503, mantendo a adesão que possa estar persistida. O consumidor não deve conectar com base nesse 503: pode repetir **o mesmo POST/UUID**, pois só adiciona uma policy sticky e não emite mensagens. Nem 503 nem timeout provam ausência da adesão.

Remover UUID da configuração, reiniciar, remover a linha da instância ou desligar queue-v1 não remove a policy. Não há API/DDL de desativação/reconciliação. Cada replay da rota ainda exige que a instância exista; policy sobrevivente sem instância não transforma 404 em sucesso.

## Capabilities e fluxo do consumidor

`GET /queue/v1/capabilities` continua usando **apikey individual** + header do contrato, com UUID derivado do contexto autenticado. Acrescenta:

```json
{
  "contract": "queue-v1",
  "instanceId": "10000000-0000-4000-8000-000000000001",
  "validUntil": true,
  "exclusiveProvisioning": true,
  "exclusiveInstance": false
}
```

Este é um **fragmento** da resposta, com os demais campos preservados. `exclusiveProvisioning:true` informa que a rota administrativa foi registrada com DB/contrato/admin middleware e o gate instalado está consultável. Não significa que aquele UUID já aderiu: antes da adesão `exclusiveInstance` pode ser false. A consulta per-instância dá true somente para sua própria policy persistida. Erro do gate dá 503, sem declaração positiva. Rotas Go isoladas registradas sem suporte admin anunciam `exclusiveProvisioning:false`.

Fluxo futuro do SaaS, não implementado nem executado aqui:

1. Criar nova instância user/admin pelo endpoint legado **inalterado**, no servidor selecionado, e confirmar/guardar UUID real e token retornados.
2. No **mesmo servidor**, usar global key existente para o POST de adesão; exigir contrato, UUID exato e `exclusiveInstance === true`.
3. Conferir capabilities autenticadas com o token daquela instância: contrato/UUID, `validUntil === true`, `exclusiveProvisioning === true` e `exclusiveInstance === true`.
4. Somente depois conectar e prosseguir com o fluxo queue-v1. Sem fallback legado ou inferência de exclusividade por nome/telefone/configuração do consumidor.

Não há hook novo em create/connect/reconnect/startup: instâncias legadas/existentes não aderem ao conectar. MASTER pode continuar usando configuração explícita já disponível. Adesão de instância antiga conectada permanece ato administrativo explícito que exige drenagem/inventário; a rota não cancela chamadas anteriores nem cerca binários/clientes externos.

### Fixture consumidor sem servidor completo

A árvore `.work/finalization/verified-final` foi atualizada para a composição testada, sem editar o `go-source` 6C compartilhado. O helper aditivo permite o Service/ledger reais com transporte falso:

```go
checker, err := queuepolicy.Initialize(ctx, sqlDB, "")
// Tratar err antes de continuar.
queuepolicy.Install(checker)

auth := auth_middleware.NewMiddleware(config, instanceService)
queuev1.RegisterRoutesWithAdmin(router, service, auth.Auth, sqlDB, auth.AuthAdmin)
```

A fixture deve possuir tabela `instances` com UUIDs confirmados; a rota não cria a instância. Usar o mesmo sqlDB no gate, ledger e provisionamento. `RegisterRoutesWithAdmin` é registro explícito de teste/integração; o runtime normal continua condicionado por `RegisterOptional(..., enabled, auth.AuthAdmin)` e pelo opt-in exato de main. `RegisterRoutes(...)` anterior continua funcionando, sem anunciar suporte administrativo.

## Arquivos e hashes

- `overlay/pkg/queuepolicy/activation.go`: adição validada e transação/DDL/lock compartilhados com configuração.
- `overlay/pkg/queuepolicy/policy.go`: reutiliza persistência compartilhada; sinal de instalação real do gate.
- `overlay/pkg/queuev1/provisioning.go`: única rota administrativa, prova pós-commit e helper de fixture.
- `overlay/pkg/queuev1/http.go`: registro opt-in com AuthAdmin aditivo e capability `exclusiveProvisioning`.
- `overlay/pkg/queuepolicy/activation_test.go`, `overlay/pkg/queuev1/provisioning_test.go`: regressões permanentes.
- `patches/exclusive-instances.patch`: wiring do AuthAdmin real em main; o restante dos guards/IDOR anteriores é preservado.
- `Dockerfile`: SHA-256 atualizado do patch funcional: **`cf8ce370d3e54c3386e2dab0f878d0c7285c0c46a304e20724cac842ef8b7427`**.
- `README.md`, `docs/queue-v1.md`, `docs/queue-v1-finalization.md`, este documento e registro de compatibilidade atualizados. Nenhuma alteração em `.github/workflows/build.yml`.

PR154, webhook, credential-logs.patch, queue-v1.patch, go.mod/go.sum e contrato de validade/hash anteriores não foram alterados. Não há migration nova de sessões/instâncias/ledger nem dependência adicional.

## Validação

Composição limpa em `.work/provisioning/composed`: baseline recomposto já verificado → logs → overlay atual → queue wiring → novo patch funcional → testes. `git apply --check`/`git apply`, sem index/commits; descoberta do Git pai bloqueada com `GIT_CEILING_DIRECTORIES`. Ferramentas locais `evolution-go-jupiter:webhook-build-test`, entrypoint `/bin/sh`, Go offline (`GOPROXY=off`, `GOWORK=off`, módulos readonly). PostgreSQL16 descartável em tmpfs/rede Docker internal, sem portas publicadas ou dados de produção.

- **Focados** queuepolicy + queuev1 + instance/handler, `-race -count=3 -parallel=1`: **1002 aprovações contando as repetições**, zero falhas/skips.
- **Full Go**, `-race -parallel=1`, PostgreSQL e prova estrita AST/baseline: **463 testes/subtestes aprovados**, zero falhas, um skip legado PDF (`pdftoppm` ausente). Nenhum skip queue/policy/provisioning/PG.
- `go vet -mod=readonly ./...`, `go mod verify` e build CGO amd64 com ldflags da candidata: aprovados; binário não executado.
- Cobertura: middleware real/global key vs tokens individuais, header/UUID, opt-in desligado, componentes/gate ausentes, todas as fases SQL e commit ambíguo, prova false/erro, concorrência/idempotência/activated_at, isolamento UUID, policy após restart/remoção da instância, ausência de rota off, campos/token/QR/conexão intactos, transporte falso sem calls, inicialização real RegisterOptional e wiring exato em main.
- Workflow comparado ao hash de entrada do usuário; preservado. Árvore `verified-final`, hashes Dockerfile e arquivos anteriores conferidos localmente.

Logs ignorados: `.work/provisioning/{input-hashes.json,offline.log,focused.json,full.json,build-vet.log,preservation.json}`. Não somar repetições focadas aos testes únicos da suíte completa. Fixtures Docker criadas para esta etapa são descartadas ao término.

**Limites:** esta mudança requer **nova imagem ARM64** para disponibilizar a rota/capability no deployment; rebuild/publicação ficam com o usuário. Não se construiu/publicou imagem, validou ARM64/runtime final ou acessou WhatsApp real. Permanecem os limites de drenagem, dependência de DB, deadline sem revogação e reconciliação de incertos da finalização anterior.
