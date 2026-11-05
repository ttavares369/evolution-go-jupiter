# Queue v1 — produtor opt-in (6A + finalização local pós-6C)

## Estado da entrega

Implementação **local**, sem commit, publicação de imagem, deploy ou envios reais. O consumidor SaaS foi integrado na finalização coordenada, documentada em `../../docs/whatsapp-queue-production-finalization.md`. Política e contrato estão em [`queue-v1-finalization.md`](queue-v1-finalization.md); cadastro administrativo em [`queue-v1-provisioning.md`](queue-v1-provisioning.md); build e execução ARM64 isolados em [`queue-v1-arm64-validation.md`](queue-v1-arm64-validation.md). Não se declara homologação WhatsApp ou sucesso em produção.

O fork é um projeto de patches/overlay. O Dockerfile compõe a fonte verificada, os patches existentes, a correção diagnóstica, o overlay e o wiring aditivo. A versão candidata é `0.7.2-jupiter3-queue-v1`, não publicada. Nenhuma configuração de produção foi alterada.

## Proveniência

| Componente | Referência |
| --- | --- |
| Evolution Go | `0.7.2`, `9337afc47e10b86cc896a6f432240e40fee95dd1` |
| Verificação upstream | 137/137 blobs Git do archive conferidos contra a árvore do commit; material local em `.work/` |
| Whatsmeow | `v0.0.0-20260630180629-b572e5bcb92b`, fonte exata inspecionada |
| `go.mod` SHA-256 | `0f05320607359b04aeb3ede7044997e94ffcc44ee3836ec7cc2336826d82810b` |
| `go.sum` SHA-256 | `525d34881251d3585e5efce44f3d015df34ccf099716ed8c0259f9d7a241670f` |
| PR154 preservado | `fcccb9f9ad5cd40332fd96a72d41f954e7c588e5c585137d648d6996e5c628a5` |
| Webhook preservado | `c163460384386317330b4420d41f8ff67a9874e01fca247624ed9f8a5b02921d` |
| `credential-logs.patch` | `c746a83caf424ebbcc35eba867919466a66fc4b5035bdfb4c4a00e600eb3e787` |
| `queue-v1.patch` | `22a65d1172c9c13d6b3a35b9c99366e68fc0d278b058d8c4647a70c538d29d43` |
| `exclusive-instances.patch` | `cf8ce370d3e54c3386e2dab0f878d0c7285c0c46a304e20724cac842ef8b7427` |

Ordem: upstream → PR154 (três mensagens aplicadas sequencialmente com `git apply`, sem index/commits) → webhook → baseline de compatibilidade → logs → overlay → wiring → exclusividade/IDOR → testes. A reprodução local dos patches/overlay produziu 73 arquivos Go idênticos à árvore testada. Dependências não foram atualizadas. O PR174 permanece excluído pelo risco de fechar o auth pool compartilhado ao combiná-lo com PR154.

A imagem ARM64 final foi construída e executada localmente sob QEMU, com CGO, PostgreSQL descartável e smokes; não foi publicada. Build amd64 passou. Imagens base/pacotes Alpine não estão todos fixados por digest: não há alegação de reprodução bit a bit ou homologação nativa Oracle. Limites de licença/instalação estão no relatório ARM64.

## Opt-in e compatibilidade

`EVOLUTION_QUEUE_V1=true` ativa **as rotas e o ledger** queue-v1, com comparação exata. Por padrão não registra essas rotas, não instancia ledger, não executa seu DDL, não inicia clientes ou workers novos. As migrações e conexões **já existentes no legado** permanecem como antes.

`EVOLUTION_QUEUE_V1_EXCLUSIVE_INSTANCE_IDS` é uma allowlist explícita de UUIDs canônicos separados por vírgula. Persiste **somente adesões** na tabela própria `queue_v1_exclusive_instances`; retirar um UUID da variável, reiniciar ou desligar `EVOLUTION_QUEUE_V1` **não libera** os emissores legados. O gate é instalado antes dos runtimes, independentemente das rotas queue. Sem allowlist e sem tabela previamente existente, o gate apenas verifica a ausência da política, sem DDL. Além da configuração explícita (por exemplo MASTER), o opt-in do contrato permite a adesão administrativa mínima `POST /queue/v1/instances/:instanceId/exclusive`, com AuthAdmin real e comprovação do gate. Não há desativação automática/API de remoção. Novos cadastros não precisam de edição dessa allowlist: o consumidor pode aderir após criação confirmada e antes de conectar, no mesmo servidor. Create/connect legados não ativam a política automaticamente. Não usar nome/telefone/token no lugar do UUID.

A política é consultada no PostgreSQL por UUID, sem cache negativo de membros. Falha de inicialização/consulta bloqueia as emissões de aplicação; não é ignorada para proteger disponibilidade legada. Isso torna a saúde desse banco uma dependência do gate, inclusive para UUIDs fora da política durante indisponibilidade. Com a política saudável, instâncias legadas/n8n não listadas mantêm seus caminhos originais.

Com opt-in, usa o pool PostgreSQL GORM já existente, com afinidade de sessão. `Migrate` tem timeout de 10 s. Falha na inicialização mantém somente o novo contrato indisponível (HTTP 503), sem abortar o startup legado. Requer correção da causa e reinicialização autorizada para tentar inicializar novamente. Não há fallback de envio.

Os emissores legados **não são redirecionados** para queue-v1. Nas exclusivas, guards HTTP/serviço impedem send/status/mídia, reaction/edit/revoke/presença, rejeição automática/manual de chamada e mutações administrativas de grupo/comunidade/newsletter/perfil, antes de preparação externa. Receipts, mark-read/played, histórico Peer e retransmissões técnicas permanecem fora do gate. Advanced settings agora exige igualdade entre UUID da URL e contexto autenticado (GET/PUT); autenticação, tokens e sessões não foram trocados. As exceções deliberadas à prova legada são o patch funcional explícito e logging sem segredos; detalhes em [`../tests/credential-logs-audit.md`](../tests/credential-logs-audit.md).

## HTTP

As rotas per-instância de capabilities/presence/text/image/attempts exigem `apikey` individual pelo middleware legado `Auth`; o UUID vem do contexto autenticado, nunca do body. A única rota administrativa adicional, `POST /queue/v1/instances/:instanceId/exclusive`, exige a **global key existente com `AuthAdmin` real**, valida o UUID da URL no banco deste servidor e somente adiciona policy — não envia mensagens nem conecta clientes. Todas exigem `X-Evolution-Queue-Contract: queue-v1`; esse header confirma contrato, não é credencial. Respostas têm `Cache-Control: no-store`.

| Método | Endpoint | Resultado |
| --- | --- | --- |
| GET | `/queue/v1/capabilities` | Capacidades, UUID autenticado, limites e semântica |
| POST | `/queue/v1/presence` | available/composing por 8–26 s, refresh e paused |
| POST | `/queue/v1/text` | Uma autorização durável de texto |
| POST | `/queue/v1/image` | Uma autorização durável de imagem base64 |
| GET | `/queue/v1/attempts/:attemptId` | Consulta, nunca executa/reenvia |
| POST | `/queue/v1/instances/:instanceId/exclusive` | **AuthAdmin**, adesão sticky de UUID existente; 200 `{contract, instanceId, exclusiveInstance:true}` somente após commit + consulta positiva do gate |

A adesão administrativa não requer body; UUID vem apenas do path. Erros específicos: 400 `invalid_instance_id`, 404 `instance_not_found`; indisponibilidade/commit ambíguo/prova falha dá 503 `queue_unavailable`, sem declarar true. Header incorreto dá 428 `queue_contract_opt_in_required`; AuthAdmin mantém 401 `not authorized`. Detalhes em [`queue-v1-provisioning.md`](queue-v1-provisioning.md).

Nos envios/presença: corpo JSON, máximo de 24 MiB, campos desconhecidos e documentos adicionais rejeitados. `kind` é opcional no HTTP: o endpoint o determina; se fornecido deve corresponder. Os erros são códigos genéricos sem strings SQL/provider/request/credenciais.

Exemplo de texto (identidades **de demonstração**, sem executar):

```json
{
  "attemptId": "10000000-0000-4000-8000-000000000001",
  "logicalMessageId": "20000000-0000-4000-8000-000000000001",
  "messageId": "3EB000000000000000001",
  "to": "5511999999999@s.whatsapp.net",
  "text": "Mensagem de demonstração"
}
```

Imagem usa as mesmas identidades/destino e `imageBase64`, `imageSha256`, `mime`, `caption`; `text` vazio. Presença usa `{"to":"5511999999999@s.whatsapp.net","durationMs":8000}`. Não enviar esse conteúdo a uma instância real para testar.

| HTTP | Interpretação |
| --- | --- |
| 200 POST texto/imagem | Envelope `{contract, attempt}`, `attempt.state=accepted`, recibo persistido |
| 202 POST texto/imagem | `preparing`, `sending` ou `uncertain`; **não é sucesso comercial** |
| 422 POST texto/imagem | `rejected`; interpretar código e mensagem lógica anterior |
| 200 GET tentativa | Estado encontrado, inclusive pendente/incerto/rejeitado; não significa aceite |
| 428 | Falta opt-in explícito |
| 401 | Auth/contexto de instância inválidos |
| 400 / 413 / 415 | Validação / corpo excessivo / Content-Type incompatível |
| 404 | Attempt não encontrado no escopo da instância |
| 409 | Conflito de identidade ou instância ocupada na presença |
| 503 | Ledger/contrato indisponível; **não comprova ausência de efeito** |

Capabilities acrescenta **`validUntil: true`** e **`exclusiveInstance: boolean`**, este último verdadeiro somente para o UUID autenticado com política persistida e gate instalado/operante. Erro ao conferir o gate dá 503, nunca uma declaração positiva. O consumidor novo deve exigir ambos `true`; queue-v1 não exige exclusividade para continuar atendendo consumidores v1 anteriores. O aditivo de cadastro acrescenta **`exclusiveProvisioning: boolean`**: true somente com a rota administrativa registrada/DB/contrato disponíveis e gate instalado consultável; não significa que a instância já aderiu. Instâncias recém-criadas podem retornar provisioning true e exclusiveInstance false até a adesão. Fixture com `RegisterRoutes(...)` anterior continua com provisioning false; usar `RegisterRoutesWithAdmin(...)` para exercitar adesão real. Os campos anteriores, inclusive `serializationScope=queue-v1-only`, são preservados aditivamente.

Capabilities publica `durableLedger`, `singleCommercialCall`, `technicalRetransmissions`, `acceptedMeans=transport_ack_not_delivery`, `uncertainReplay=false`, `serializationScope=queue-v1-only`, tipos/limites de mídia e duração de presença. Não publica nome, token, sessão ou corpo. É descoberta de contrato inicializado, não garantia de disponibilidade futura do DB/transporte.

## Validação e identidade

- UUIDs canônicos não nulos: instância, attempt e mensagem lógica.
- `messageId`: 1–64 caracteres ASCII alfanuméricos, estável e fornecido ao Whatsmeow.
- `to`: JID canônico PN (`s.whatsapp.net`), LID (`lid`) ou grupo (`g.us`), até 255 bytes, sem device/agent/integrator. Não suporta newsletter, peer/bot ou protobuf arbitrário.
- Texto UTF-8 não vazio, até 64 KiB. Não pode conter campos de imagem/legenda.
- Legenda UTF-8 até 4096 bytes.
- Imagem base64 padrão estrito com padding, sem CR/LF, URL ou data URI. Até 16 MiB, MIME real JPEG/PNG/WebP, SHA-256 dos bytes em hexadecimal minúsculo. `DecodeConfig` valida dimensões até 8192 por lado e 32 milhões de pixels. Não é uma varredura antivírus ou uma decodificação integral de todo arquivo.
- `validUntil?: string`: somente UTC canônico ISO com exatamente três casas decimais, por exemplo `2026-11-05T13:00:00.000Z`. Ausente significa **sem deadline**; HTTP rejeita `null`, string vazia, offsets e outras precisões. Não é `maxDelay` de lembretes. O produtor não inventa nem deriva deadline quando ausente. `Attempt.validUntil` é devolvido somente quando definido e `Binding`/ledger o preservam imutavelmente.
- Owner é UUID aleatório do servidor, nunca aceito do consumidor nem exposto no JSON.

O hash SHA-256 do conteúdo é calculado sobre os bytes JSON do Go para a tupla fixa:

```text
["queue-v1/content/1",kind,to,text,caption,mime,imageSha256]
```

**Sem deadline, essa tupla7 permanece byte a byte igual.** Com `validUntil`, acrescentar o string canônico como oitavo elemento, sem mudar o prefixo:

```text
["queue-v1/content/1",kind,to,text,caption,mime,imageSha256,validUntil]
```

Alterar/adicionar/remover deadline muda a identidade/hash e conflita com o vínculo anterior, inclusive em novo attempt da mesma mensagem lógica. Vetores compartilhados estão em `overlay/pkg/queuev1/deadline_test.go` e no registro de finalização.

O consumidor futuro deve reproduzir o escaping de `encoding/json`, inclusive HTML/U+2028/U+2029, com vetores compartilhados. IDs e representação base64 não entram no hash; o SHA da imagem representa seus bytes. O ledger armazena vínculos/hash, não corpos, mídia, tokens ou chaves. Hashes podem permitir inferência de conteúdos de baixa entropia: acesso/retenção/backups continuam sensíveis.

## Ledger e exclusão mútua

Quatro tabelas próprias, sem alteração/FK/cascade em instâncias, sessões ou histórico legado:

- `queue_v1_schema`: versão 2, singleton; upgrade transacional de v1 preserva todos os vínculos, estados e receipts, com deadline ausente nas linhas antigas.
- `queue_v1_instances`: barreira durável da instância, attempt/owner ativos.
- `queue_v1_operations`: mensagem lógica ligada ao `messageId`, hash e attempt corrente.
- `queue_v1_attempts`: PK `(instance_id, attempt_id)`, estado, timestamps e recibo.

`UNIQUE(instance_id,message_id)` e vínculos compostos impedem reaproveitar ID para outra operação/conteúdo. DDL é explícito, idempotente e transacional; versão desconhecida falha fechada. Não há downgrade/limpeza automática.

Advisory lock **de sessão física** por instância serializa presença/preparação/envio queue-v1; instâncias diferentes podem trabalhar em paralelo. Não usar PgBouncer em transaction pooling. Há orçamento local de até quatro sessões, reduzido para reservar conexão do pool; não é autoridade de idempotência. Unlock não confirmado descarta a conexão física, nunca a devolve ao pool com lock possivelmente vivo.

Locks de linha seguem instância → operação → attempt. Admissão, autorização e conclusão usam transações curtas, inclusive na mesma conexão scoped. Durante upload/presença/envio existe sessão, **não transação SQL aberta**. HTTP limita esperas de admissão a 5 s; após commit da admissão, a operação possui contexto próprio (85 s), trabalho de até 70 s e reserva para bookkeeping. Upload/envio usam limite de contexto de 25 s. Não são garantias de interromper instantaneamente o socket/dependência.

A política exclusiva usa uma quinta tabela independente, `queue_v1_exclusive_instances`, sem FK/mutação de sessões/instâncias/tokens. A migração do ledger não a desativa.

Sequência: `Admit → check deadline → Prepare → check deadline → MarkSending commit → check deadline → Prepared.Send/check final → SendMessage único → Complete commit → resposta`. Upload de mídia ocorre na preparação, mas não envia mensagem comercial. Falha de preparação é rejeição pré-send; erro/commit ambíguo de `MarkSending` impede chamar o transporte e conserva barreira.

## Idempotência e estados

Mesmo attempt + vínculo idêntico retorna o registro sem nova preparação/envio; conteúdo/identidade divergente dá conflito. Outra tentativa da mesma mensagem lógica pendente/aceita não ganha execução. Após rejeição comprovadamente pré-send, **outro attempt** pode ser admitido, mantendo mensagem lógica, hash e `messageId`. Isso não habilita retry automático no SaaS.

| Estado | Significado |
| --- | --- |
| `preparing` | Admissão persistida; não retoma automaticamente após crash |
| `sending` | Autorização persistida antes do transporte; não prova escrita nem ausência dela |
| `accepted` | Recibo com ID correspondente e timestamp, persistido; **ACK, não entrega/leitura** |
| `rejected` | Esta tentativa não chegou à chamada comercial; não nega efeito de tentativa anterior |
| `uncertain` | Efeito/recibo incerto, mantém barreira de instância e operação |

Códigos: `instance_busy`, `logical_pending`, `logical_accepted`, `preparation_failed`, **`expired`**, `send_uncertain`, `receipt_mismatch`.

`expired` só é rejeição durável da tentativa quando comprovadamente **antes** da chamada comercial. A igualdade `now == validUntil` já expira. O transporte confere novamente imediatamente antes de `Client.SendMessage`. Uma rejeição `expired` pode ter `sendingAt` definido, pois esse timestamp indica a autorização persistida, não entrada comprovada na chamada. Apenas o owner síncrono pode finalizar esse caso pré-chamada; sobreviventes/incertos nunca são liberados por idade/deadline. Depois de entrar em `SendMessage`, erro/timeout segue incerto e ACK válido segue aceito, mesmo após a expiração. Replay após expiry devolve o resultado original; não reclassifica aceite ou barreira pendente. **Novo attempt rejeitado por `logical_pending`/`logical_accepted` NÃO comprova que a mensagem original não foi enviada.** Nunca usar para liberar outbox, inventar ID ou cair no legado.

Concorrência pode persistir uma rejeição `instance_busy` antes de outro request do mesmo attempt ganhar admissão. É conservador: o mesmo attempt permanece rejeitado; não é reexecutado depois. Eventual nova autorização exige outro attempt e prova pré-send apropriada.

Falha SQL após transporte ou unlock ambíguo retorna indisponibilidade, não falso sucesso. Após resposta perdida, consultar/repetir **a identidade original**. `404` não prova ausência remota. Sobreviventes `preparing/sending/uncertain` permanecem bloqueados após reinício, sem TTL, owner novo ou replay. O ledger permite ao owner original registrar ACK positivo correlacionado; não há endpoint de reconciliação/desbloqueio nesta etapa. Negativa por silêncio/timeout/ausência de receipt nunca é prova suficiente.

## Presença e lifecycle

Presença separada exige 8000–26000 ms, lock de instância e `Idle` sem barreira pendente. Emite available/composing, renova a cada no máximo 5 s, tenta paused no final e não confirma sucesso se uma dessas operações falhar. Texto/imagem não acrescentam typing implícito. O consumidor deve respeitar presença confirmada e seus intervalos; o produtor não é o scheduler comercial.

Lease usa cliente/runtime já existente; não inicia/reconecta/modifica sessões. Cancela quando runtime encerra, mas só libera worker/store após a chamada real retornar. HTTP abortado depois da admissão não abandona Send em goroutine. Cancelar contexto não prova ausência de frame em trânsito/ACK tardio.

## Retransmissões técnicas preservadas

B5 original: wrappers legados de texto/mídia têm retries de aplicação e geram/retornam identidade fora de um ledger durável; uma resposta perdida não oferece contrato seguro de tentativa única. Queue-v1 não os chama, nem modifica seus loops.

Na revisão exata do Whatsmeow:

- `send.go` usa `SendRequestExtra.ID`; sem ele geraria ID. V1 sempre fornece o ID persistido.
- `request.go/retryFrame` pode reenviar **o mesmo ID e os mesmos bytes da stanza** após reconexão.
- `retry.go` trata retry receipts, recupera/recriptografa o payload com **o mesmo messageID**, podendo usar novos bytes criptografados. Não é nova autorização comercial.
- Cache recente de 256 entradas não é deduplicação durável. A integração não habilita `UseRetryMessageStore`; crash/evicção pode perder payload para retransmissão técnica. O ledger não guarda corpo/chaves e não corrige isso reenviando comercialmente.

Não há patch global do Whatsmeow. Não se promete um único frame ou exactly-once remoto. Texto/imagem novos chamam `Client.SendMessage` no máximo uma vez por autorização.

## Evidências locais e limites

Os registros e contagens abaixo são o histórico **6A**. A finalização pós-6C recompôs novamente os fontes, preservou esse trabalho e executou a suíte atual; resultados e arquivos em [`queue-v1-finalization.md`](queue-v1-finalization.md).

Ambiente: Go 1.25.0 na imagem de ferramentas já existente `evolution-go-jupiter:webhook-build-test`; servidor dessa imagem nunca executado. Módulos offline, `GOTOOLCHAIN=local`, `GOPROXY=off`, `GOWORK=off`. PostgreSQL 16 descartável em tmpfs, rede Docker **internal**, sem portas publicadas, DSN ou dados de produção.

Registros locais ignorados em `.work/`:

- `baseline-tests.log`: testes/vet antes da composição.
- `queue-integration-tests.log`: pacote queue-v1 com race, três repetições, PostgreSQL real, zero skips.
- `final-tests.json`: suíte completa da composição reproduzida, `-race -parallel=1`, incluindo prova AST/baseline e PostgreSQL real: **348 testes/subtestes aprovados, zero falhas, um skip legado PDF**; nenhum skip de queue-v1/PostgreSQL.
- `final-build-vet.log`: módulos verificados, vet e compilação de `./cmd/evolution-go` com ldflags da candidata; binário não executado.

Cobertura: texto/imagem, replay/conflitos, concorrência entre pools, instâncias independentes, barreiras pós-crash, falhas/commits SQL, pool/row locks/unlock, auth legado, opt-in default off, HTTP perdido/abortado, recibo inválido/incerto, presença/lease, logging sem canários e handlers legados texto/mídia. Um teste legado de thumbnail PDF é skip por ausência de `pdftoppm` na imagem de ferramentas; não envolve queue-v1.

Fakes não provam frames reais/dedup remoto. O teste Whatsmeow de retry cache/receipt para em `PreRetryCallback` antes do I/O; o de retransmissão simula IDs repetidos dentro de uma chamada. Testes PostgreSQL usam apenas `QUEUE_V1_TEST_DSN`, guard restrito a loopback IPv4/alias `queue-v1-postgres` e DB `evolution_queue_test_<suffix>`; sem isso os testes reais pulam, não aprovam banco real.

Comandos centrais, dentro da fixture isolada:

```sh
go test -mod=readonly -race -count=3 -parallel=1 -timeout=120s ./pkg/queuev1
EXCLUSIVE_INSTANCES_PATCH=/tmp/exclusive-instances.patch CREDENTIAL_LOGS_BASELINE=/baseline go test -mod=readonly -race -parallel=1 -timeout=120s ./...
go mod verify
go vet -mod=readonly ./...
CGO_ENABLED=1 go build -mod=readonly -ldflags '-X main.version=0.7.2-jupiter3-queue-v1' -o /tmp/server ./cmd/evolution-go
```

## Pendências obrigatórias de homologação

1. Validar imagem final amd64/arm64 e runtime/libs em ambiente isolado autorizado; nenhum resultado de Go build substitui isso.
2. Validar conexão/reconexão/webhooks/sessões com contas de homologação autorizadas; nenhum teste desta etapa conecta uma conta real.
3. Observar retryFrame/retry receipts, ACK tardio e identidade real de transporte sem inferir exactly-once remoto.
4. Ativar/homologar o gate persistido por UUID real e drenar produtores antigos/em voo antes do cutover. Esta imagem bloqueia os emissores de aplicação inventariados nas exclusivas; binários antigos, outra instalação/cliente ou instâncias abertas fora deste processo não são cercados magicamente. Não adicionar UUIDs n8n à política nem compartilhar a mesma conta entre emissores. O consumidor deve permanecer bloqueado sem o handshake positivo completo.
5. Definir procedimento autorizado de reconciliação de sobreviventes/incertos. Nenhum desbloqueio negativo automático foi adicionado; uma tentativa incerta pode bloquear a instância até análise com evidências.
6. Conferir logs de dependências/`WA_DEBUG`, proxies, histórico e retenção/backups. Correções do fork não saneiam logs já gravados nem todas as dependências.
7. Homologar posteriormente consumidor 6B, vetores de hash e política de consulta/rejeição. **Não liberar B5 agora.**

## Sequência futura de publicação e rollback — NÃO executar

1. Revisar/aprovar o produtor e a homologação; revisar naming do workflow `.github/workflows/build.yml` em mudança autorizada. O workflow já recebido do usuário contém naming `jupiter3-queue-v1` e mantém push `main`/workflow_dispatch; foi preservado sem edição nesta etapa. Rebuild ARM64/imagens, homologação e publicação continuam exigindo coordenação/autorização próprias. Nenhum push nesta etapa.
2. Com autorização, publicar tag/digest imutáveis novos e registrar imagem/configuração anteriores; nunca sobrescrever tags antigas.
3. Instalar em homologação com queue-v1 desabilitado, conferir legado/n8n/webhooks; só então opt-in, DDL próprio e testes do contrato. Não rotacionar tokens, recriar instâncias ou mudar sessões.
4. Produção exige nova autorização/janela. Publicar produtor não ativa o SaaS; implantar primeiro mantendo B5 fechado e consumidores legados intactos.
5. Etapa 6B futura: handshake/versionamento/auth, persistência antecipada de IDs/hash, presença e intervalos, consulta original após timeout, sem fallback legado ou IDs novos para incerteza; revalidar exclusividade e regras do SaaS antes de eventual liberação B5.
6. Rollback coordenado: parar novas admissões do consumidor, preservar outbox, IDs, quatro tabelas/owners/receipts/barreiras e backup recuperável. Operações em voo são potencialmente efetivas.
7. Só sob autorização desabilitar opt-in/restaurar digest anterior, mantendo consumidor parado; uma imagem antiga não oferece lookup, portanto planejar reconciliação compatível. Não truncar ledger, executar down migration, restaurar snapshot antigo por cima de tentativas recentes, liberar incerteza por idade ou reenfileirar em legado.
8. Retomar somente após reconciliação e aprovação expressa de ambas as pontas.
