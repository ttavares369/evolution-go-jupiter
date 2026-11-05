# Queue-v1 — bloqueadores locais pós-6C (2026-11-05)

> Registro da finalização inicial. O aditivo posterior de adesão administrativa, que remove a necessidade de editar a allowlist a cada cadastro, está em [`queue-v1-provisioning.md`](queue-v1-provisioning.md). Contagens abaixo preservam o histórico anterior; a execução atual aprova 463 testes/subtestes.

## Escopo e estado

Implementado **somente neste fork**. Sem commit, staging, branch, push, publicação, deploy, produção, conexão WhatsApp ou execução do servidor/entrypoint original. Nenhuma escrita no SaaS, no `backend/node_modules/.cache/6c-audit/go-source` compartilhado ou no baseline ao lado. Workflow de publicação intacto. Trabalho anterior preservado.

Lidos `docs/queue-v1.md` e a auditoria SaaS `../docs/whatsapp-queue-6c-audit.md`. Esta entrega resolve localmente os bloqueadores de produtor P1/P2 e o IDOR P3; **não** aprova produção nem resolve as demais pendências de runtime/publicação/reconciliação da auditoria.

## Política final e setup para consumidor/fixture

Configuração ilustrativa, **não aplicada a ambiente algum**:

```dotenv
EVOLUTION_QUEUE_V1=true
EVOLUTION_QUEUE_V1_EXCLUSIVE_INSTANCE_IDS=00000000-0000-4000-8000-000000000001
```

- A segunda variável é uma via de ativação explícita de exclusividade (por exemplo MASTER); com o aditivo admin ela não é obrigatória por cadastro novo. Recebe UUID real canônico não nulo; lista separada por vírgula, com espaços periféricos tolerados. Lista malformada falha fechada, não vira lista vazia. Nome, número, MASTER, token e flags HTTP não são seletores de política.
- Adesão persistida por UUID em `queue_v1_exclusive_instances(instance_id UUID PRIMARY KEY, activated_at)`. Apenas INSERT idempotente, sem alteração de tabelas/sessões/tokens legados. UUID duplicado é inofensivo; UUIDs diferentes são independentes.
- Política **sticky**: retirar UUID da variável, reiniciar ou desligar `EVOLUTION_QUEUE_V1` não libera o legado. O aditivo posterior permite somente adesão por AuthAdmin; não existe rotina/API de remoção/desbloqueio. Não apagar a tabela para fazer rollback.
- `queuepolicy.Configure` instala o gate antes da construção dos runtimes e antes de registrar rotas; funciona independentemente do opt-in HTTP queue. Sem variável e sem tabela existente não faz DDL: verifica a ausência da política e mantém comportamento legado. Processos observam adesões persistidas por outros processos, sem cache negativo de UUIDs.
- Falha de configuração/consulta, instalação ausente ou desaparecimento de tabela já observada fecha as emissões de aplicação. Isso pode indisponibilizar também emissores fora da política quando não se consegue determinar o escopo: não é um fallback silencioso que preserve disponibilidade à custa de exclusividade. Não derruba nem reinicia sessões.
- Consumidor novo exige GET autenticado `/queue/v1/capabilities`, header `X-Evolution-Queue-Contract: queue-v1`, UUID exato e **`validUntil === true && exclusiveInstance === true`**. `exclusiveInstance` é verdadeiro somente se o gate instalado consultar positivamente a política persistida daquele UUID; erro dá 503. Outros UUIDs dão false. O contrato queue-v1 continua utilizável pelos consumidores v1 antigos sem exigir exclusividade no servidor.
- Fixture que instancia rotas sem o `main`: inicializar/migrar o ledger; chamar `queuepolicy.Initialize(ctx, sqlDB, uuidAllowlist)`, verificar erro e instalar o checker com `queuepolicy.Install(checker)` **antes** das rotas/serviços. `Configure(gormDB, allowlist)` faz essa instalação no runtime normal. Não simular exclusividade apenas adicionando um booleano à resposta.

Antes de um cutover real, drenar workers/emissões anteriores e garantir que todos os produtores da conta usam esta implementação. O gate não cerca outro binário antigo, outro deployment, outro cliente vinculado ou uma chamada já iniciada antes de a política ser ativada.

## Inventário de emissão e compatibilidade

`patches/exclusive-instances.patch` acrescenta guards na entrada dos serviços, não apenas middleware HTTP. A recusa ocorre antes de validação que possa iniciar runtime, download, conversão, upload, typing ou envio. Guards também protegem wrappers internos de retry de texto/link/mídia/poll e o `SendMessage` central. Não há redirecionamento/fallback legado→queue.

| Caminho | Em UUID exclusivo | Fora da política, banco saudável |
| --- | --- | --- |
| `/send/*`: texto/link/mídia/poll/sticker/location/contact/button/list/carousel/status | Bloqueado, inclusive antes de baixar/enviar mídia | Implementação, DTOs, respostas e retries originais |
| Reaction, edit, revoke e presença legada | Bloqueado | Original |
| `/call/reject` e auto-reject-call, inclusive texto opcional | Bloqueado como emissão iniciada pela aplicação | Original |
| Presença periódica e de conexão/critical app-state sync | Somente esses anúncios são suprimidos; bookkeeping de conexão continua | Original |
| Criação/alteração de grupo/comunidade, participantes, join/leave/settings/photo; newsletter create/subscribe; perfil picture/name/status | Bloqueado antes de efeitos/preparação | Original |
| `/group/invitelink` | Conservadoramente bloqueado por ser rota capaz de resetar convite | Original |
| Mark-read/played, receipts, ACK, histórico Peer, sincronização/reconexão técnica e retries do Whatsmeow | Preservados; não passam pelo gate comercial | Original |
| Queue-v1 texto/imagem/presença | Contrato/ledger próprios | Contrato anterior continua disponível quando opt-in |

HTTP dos caminhos inventariados retorna 403 `queue_v1_exclusive_instance`; indisponibilidade da política retorna 503 `queue_policy_unavailable`. Chamadas diretas aos serviços também recebem erro antes do I/O. Autenticação original e protocolos técnicos não foram substituídos. Nenhum patch global do Whatsmeow.

Advanced settings: GET/PUT compara `:instanceId` da URL com `instance.Id` do contexto autenticado, com verificação de tipo/nil. Seleção de UUID alheio/sem contexto dá 403 antes de leitura, parse do update ou escrita. Configurações da própria instância continuam pelo serviço original; gate não reescreve `RejectCall`, `AlwaysOnline` ou outros settings armazenados.

## Contrato aditivo de validade

Sem mudança de endpoints, autenticação/header, `contract: queue-v1`, campos anteriores de capabilities ou dependências.

- Capabilities acrescenta `validUntil: true` e `exclusiveInstance: boolean`. `serializationScope: queue-v1-only` permanece: o ledger serializa operações queue; o gate exclui os emissores de aplicação alternativos.
- `SendRequest.validUntil?: string`, somente UTC canônico com exatamente milissegundos, como `2026-11-05T13:00:00.000Z`. HTTP rejeita null, vazio, offsets, precisão diferente e data inválida. O decoder continua rejeitando campos desconhecidos.
- Ausente significa **sem deadline**: não acrescentar campo no request/attempt, não inferir de maxDelay, prazo de início de lembrete, timeout HTTP ou idade do job. Não usar prazo de ocorrência como validade do lote iniciado.
- `Binding`, operação lógica, tentativa e `Attempt.validUntil` preservam o string canônico. Alterar/adicionar/remover deadline conflita com identidade existente, inclusive em outro attempt da mesma mensagem lógica. JSON da tentativa omite o campo quando ausente.
- Hash sem deadline mantém a tupla7 exata; com deadline acrescenta o oitavo elemento canônico, sem trocar `queue-v1/content/1`. Continuidade do escaping `encoding/json` (HTML/U+2028/U+2029).

Vetores permanentes em `deadline_test.go`, SHA-256 dos bytes UTF-8 compactos:

```text
["queue-v1/content/1","text","5511999999999@s.whatsapp.net","hello","","",""]
3c97e4881583e1cc954e7463d0e3b245967b0880927a4ac8996bedb92bb16c56

["queue-v1/content/1","text","5511999999999@s.whatsapp.net","hello","","","","2026-11-05T13:00:00.000Z"]
60c779fddefad7cd7a56223c268409bde210b77b71a214992af47710de54e51e
```

### Fronteira de efeito

Checks com relógio injetável: após admissão e antes de Prepare; depois de Prepare (inclusive upload/falha de preparação); após `MarkSending` e imediatamente antes de `Prepared.Send`; no transporte antes de upload, depois da preparação e na última instrução de aplicação antes de `Client.SendMessage` (após clone do protobuf/readiness). `now >= validUntil` expira.

Rejeição durável `state=rejected, code=expired` somente com prova pré-chamada. Pode conservar `sendingAt` se o guard final evitou a chamada depois da autorização persistida; esse timestamp não é prova de escrita no socket. O owner síncrono pode registrar essa negativa; MarkSending/SQL ambíguo conserva a barreira.

Após entrar em SendMessage, expiry não é prova negativa. Erro/timeout/receipt inválido continua incerto; ACK válido continua aceito. Erros originados na chamada são envolvidos em `ErrAmbiguousSend`, inclusive um erro que carregue `ErrExpired`, para impedir reclassificação acidental. A tentativa original após expiry é replayada sem nova preparação/envio; accepted/preparing/sending/uncertain nunca são liberados por idade/deadline. Uma tentativa nova expirada também não esconde `logical_accepted`/`logical_pending` anteriores.

Isso não cria revogação, cancelamento remoto, exactly-once, deadline dentro da biblioteca/socket ou garantia de entrega antes do prazo. Existe a inevitável fronteira entre o último check de relógio e a entrada na função da dependência; não há alegação de interrupção de uma mensagem em voo.

### Migração própria

Ledger passa de schema 1 para **2**, atomicamente sob o lock de migration existente, sem down destrutivo. `valid_until TEXT NOT NULL DEFAULT ''` em operações/tentativas: vazio é ausência para todas as linhas v1, sem backfill de data. FK composta inclui deadline; operação e tentativa não podem divergir. Constraint de estado permite exclusivamente a negativa `expired` pré-chamada após `sendingAt`; uncertain não pode virar rejected por expiry. Versão desconhecida falha fechada. Tabela de política é independente e só recebe DDL quando há adesão explícita.

## Arquivos desta finalização

- **Novo patch:** `patches/exclusive-instances.patch`, SHA-256 `cf8ce370d3e54c3386e2dab0f878d0c7285c0c46a304e20724cac842ef8b7427` (inclui o wiring posterior de AuthAdmin; hash atualizado no Dockerfile).
  - Wiring: `cmd/evolution-go/main.go`, `pkg/routes/routes.go`.
  - Guards: serviços de sendMessage, message, call, group, community, user, newsletter e `pkg/whatsmeow/service/whatsmeow.go`.
  - IDOR: `pkg/instance/handler/instance_handler.go`.
- **Novo overlay de política:** `overlay/pkg/queuepolicy/{policy.go,runtime.go,policy_test.go}`.
- **Contrato/ledger/transporte alterados:** `overlay/pkg/queuev1/{types.go,service.go,http.go,transport.go,postgres.go}`.
- **Novos arquivos de validade:** `overlay/pkg/queuev1/{deadline.go,request_json.go,deadline_migration.go,deadline_test.go,deadline_migration_test.go}`.
- **Novos testes de gate:** `overlay/pkg/queuev1/{exclusivity_test.go,exclusivity_inventory_test.go}`.
- **Teste de IDOR:** `overlay/pkg/instance/handler/advanced_settings_scope_test.go`.
- **Fixtures existentes adaptadas, sem remover regressões:** `overlay/pkg/queuev1/{service_test.go,postgres_test.go,admission_safety_test.go}`.
- **Prova legada:** `tests/credential_logs_test.go` reverte exatamente o novo patch funcional em cópia temporária e então executa a comparação estrita AST/bytes anterior. Exige `EXCLUSIVE_INSTANCES_PATCH` junto do baseline. Não normaliza guards arbitrários nem modifica a árvore testada/baseline.
- **Receita/documentação:** `Dockerfile`, `README.md`, `docs/queue-v1.md`, este registro e `tests/credential-logs-audit.md`.

`credential-logs.patch`, `queue-v1.patch`, PR154, webhook, módulos e workflow não foram alterados. Dockerfile confere o hash novo e aplica as três mensagens PR154 com `git mailsplit` + `git apply --check`/`git apply`, sem `git am`, index ou commits. O baseline resultante continua byte a byte igual ao da auditoria 6C.

## Validações e reprodução local

Fonte inicial copiada da 6C para **nova** `.work/finalization/source`; nenhuma edição no cache compartilhado. Depois, composição independente do archive local: 137 blobs conferidos por SHA-1 Git contra a árvore `9337afc47e10b86cc896a6f432240e40fee95dd1` → PR154 → webhook → baseline → logs → overlay atual → queue wiring → novo patch → testes. Baseline recomposto sem diferenças em relação à cópia da 6C; árvore final em `.work/finalization/verified-final`.

Cuidado: dentro de subpastas do fork, `git apply` pode descobrir o repositório pai e ignorar caminhos. A composição usa `GIT_CEILING_DIRECTORIES` no pai do scratch (ou diretório fora de qualquer repo), e confere os fontes. Não se testou uma árvore `.work` antiga sem overlay atual.

Ambiente: imagem local de ferramentas `evolution-go-jupiter:webhook-build-test`, entrypoint substituído por `/bin/sh`, Go1.25, `GOPROXY=off`, `GOWORK=off`, módulos readonly. PostgreSQL16 descartável em tmpfs, rede Docker **internal**, sem portas publicadas, somente credenciais/dados de fixture. Nenhum container preexistente alterado.

| Validação | Resultado |
| --- | --- |
| Suíte Go completa, árvore final recomposta, `-race -parallel=1`, PostgreSQL real e prova AST/baseline | **439 testes/subtestes aprovados**, zero falhas; um skip legado PDF por `pdftoppm` ausente; nenhum skip queue/policy/IDOR/PG |
| Pacotes queuev1, queuepolicy e instance/handler, PostgreSQL + race, três repetições | Aprovados |
| `go vet -mod=readonly ./...` | Aprovado |
| `go mod verify` | `all modules verified` |
| `CGO_ENABLED=1 go build -mod=readonly` com ldflags da candidata | Aprovado amd64; binário não executado |
| Reaplicação completa dos patches sem index/commits e comparação do baseline | Aprovada; 137 blobs upstream verificados |
| `git diff --check`, hashes Dockerfile e preservação dos arquivos anteriores fora do conjunto desta etapa | Conferidos localmente |

Comandos Go centrais, dentro da árvore isolada, com os mounts/DSN de fixture descritos:

```sh
go test -mod=readonly -race -count=3 -parallel=1 -timeout=180s ./pkg/queuev1 ./pkg/queuepolicy ./pkg/instance/handler
EXCLUSIVE_INSTANCES_PATCH=/tmp/exclusive-instances.patch CREDENTIAL_LOGS_BASELINE=/baseline go test -mod=readonly -race -parallel=1 -timeout=180s ./...
go vet -mod=readonly ./...
go mod verify
CGO_ENABLED=1 go build -mod=readonly -ldflags '-X main.version=0.7.2-jupiter3-queue-v1' -o /tmp/server ./cmd/evolution-go
```

Logs/baselines locais ignorados: `.work/finalization/{initial-hashes.json,validated-final.json,final-focused.json,final-build-vet.log,...}`. As contagens não devem ser somadas entre execuções sobrepostas. Houve falhas intermediárias corrigidas de fixtures sqlmock com colunas novas e um import não utilizado; a tentativa inicial de composição que descobriu o Git pai foi descartada, não usada como evidência final.

## Limitações e pendências preservadas

- Sem imagem final Docker construída/runtime testado, sem ARM64, sem contas WhatsApp, entrega real, sessão real, proxy de produção ou publicação. Imagens base/pacotes ainda não são todos fixados por digest.
- Consumidor/fixture cruzada TS+Go será coordenado separadamente; não se modificou nem executou o SaaS nesta entrega. Os vetores, capabilities e regras acima são o handoff.
- Gate depende de PostgreSQL saudável e de todos os emissores relevantes rodarem o novo código. Ativação não cancela I/O anterior. Drenagem/inventário são requisitos operacionais, não substitutos para o gate persistido.
- Não desativar política nem apagar tabelas/IDs/receipts/barreiras em rollback. Desativação administrativa/reconciliação negativa exige desenho e autorização específicos. Nenhuma recuperação por TTL.
- ValidUntil não revoga conteúdo por pagamento, troca de token ou cancelamento posterior; não garante validade no momento de entrega/leitura. Relógio do host precisa de sincronização operacional.
- Logs/WA_DEBUG/dependências, retenção, multiarch, credencial queue dedicada e homologação das retransmissões continuam pendências. Nenhum token atual foi rotacionado para simular isolamento.
