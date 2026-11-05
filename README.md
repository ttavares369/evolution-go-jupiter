# evolution-go-jupiter — www.jupiterti.com

Build customizada da Evolution API GO, com lifecycle PR154, webhook autenticado do LembrAI e contrato **opt-in** queue-v1. Esta revisão é **local, não publicada**; nenhuma instância/sessão/token de produção foi alterada.

## Base verificada

- Upstream: `evolution-foundation/evolution-go`, `0.7.2`.
- Commit exato: `9337afc47e10b86cc896a6f432240e40fee95dd1`.
- Whatsmeow: `v0.0.0-20260630180629-b572e5bcb92b`, dependência não alterada.
- Archive upstream: 137/137 blobs Git conferidos contra a árvore do commit.
- PR154 e `lembrai-webhook.patch` preexistentes preservados.

## Candidata e publicação

O Dockerfile local compõe **`0.7.2-jupiter3-queue-v1`**, com `VERSION`/ldflags coerentes, patches verificados por SHA-256, overlay e testes. Não sobrescrever as referências anteriores `jupiter1`/`jupiter2-webhook`.

**O workflow `.github/workflows/build.yml` foi preservado sem edição nesta etapa.** A edição recebida do usuário já usa tags `jupiter3-queue-v1` e mantém push para `main`/workflow_dispatch. A nova rota de adesão exige rebuild da imagem ARM64, a ser coordenado pelo usuário. Registrar digest multiarch e configuração anterior para rollback; nenhuma publicação/push/deploy foi executada aqui.

## Queue-v1 — 6A + bloqueadores locais pós-6C

Contrato: **[`docs/queue-v1.md`](docs/queue-v1.md)**. Inventário/validade: **[`docs/queue-v1-finalization.md`](docs/queue-v1-finalization.md)**. Cadastro sem edição de allowlist, respostas exatas e fixture atual: **[`docs/queue-v1-provisioning.md`](docs/queue-v1-provisioning.md)**.

- Desabilitado por padrão; somente `EVOLUTION_QUEUE_V1=true` registra rotas e inicializa seu ledger.
- Sem opt-in do contrato: zero rotas/DDL/clientes/workers adicionais do ledger queue-v1. O gate de exclusividade é independente e sempre instalado antes dos runtimes.
- Endpoints: `GET /queue/v1/capabilities`, `POST /queue/v1/presence`, `POST /queue/v1/text`, `POST /queue/v1/image`, `GET /queue/v1/attempts/:attemptId`.
- Rotas per-instância exigem auth legada individual `apikey` + `X-Evolution-Queue-Contract: queue-v1`; UUID vem do contexto autenticado. A adesão administrativa usa o mesmo header, AuthAdmin/global key e UUID existente no path, nunca no body.
- Ledger PostgreSQL próprio, quatro tabelas `queue_v1_*`, upgrade v1→v2 transacional no opt-in. Não reescreve receipts/identidades nem inventa deadline nas linhas antigas. Falha fecha o contrato (503), sem fallback.
- Uma chamada comercial por autorização; retry HTTP repete resultado, não mensagem. `accepted` é ACK persistido, não entrega. `uncertain`/sobreviventes de crash não autorizam replay ou desbloqueio negativo.
- Retransmissões técnicas Whatsmeow com mesma identidade preservadas; sem alteração global da dependência.
- `EVOLUTION_QUEUE_V1_EXCLUSIVE_INSTANCE_IDS`: UUIDs canônicos separados por vírgula; adesão persistente PostgreSQL em uma quinta tabela própria. Nas exclusivas, bloqueia emissores legados antes de preparação/I/O, presença concorrente, auto-reject-call e mutações administrativas. Remover da variável/desligar o contrato não libera o legado. Novos cadastros podem aderir por `POST /queue/v1/instances/:instanceId/exclusive`, com AuthAdmin/global key existentes, UUID já criado no mesmo DB e prova positiva do gate após commit. Não há rota para desativar.
- Fora da política, legado/n8n mantém os caminhos originais com o banco saudável. Falha de inicialização/consulta do gate bloqueia emissões de aplicação, inclusive quando não é possível determinar a política do UUID.
- Capabilities acrescenta `validUntil:true`, `exclusiveInstance:boolean` (UUID autenticado realmente protegido) e `exclusiveProvisioning:boolean` (rota admin/gate disponíveis). Para novos user/admin: criação confirmada → adesão admin → handshake per-instância → conexão; nenhuma ativação automática em create/connect. MASTER pode usar configuração explícita.
- `validUntil?:string` UTC canônico com milissegundos: identidade/hash/ledger imutáveis; tupla7 original sem deadline, tupla8 com deadline. Expiração durável `expired` somente comprovadamente pré-chamada; incerteza/aceite não são liberados/reclassificados por expiry.
- IDOR de advanced settings corrigido comparando URL e instância autenticada. Sessões/tokens preservados.
- Consumidor/ativação operacional são entregas separadas. Não há aprovação de produção por testes locais.

## Logs seguros

[`tests/credential-logs-audit.md`](tests/credential-logs-audit.md) descreve a correção diagnóstica: tokens, DSN, QR, URLs assinadas e dumps sensíveis removidos; access log por template de rota e recovery sem payload/headers. Credenciais não são rotacionadas. Prova AST compara declarações legadas após reverter exatamente o novo patch funcional numa cópia temporária; preserva as exceções estritas de diagnóstico/wiring e a comparação byte a byte das demais fontes.

Logs antigos e logs de dependências/proxies não são saneados por esse patch. `WA_DEBUG`, retenção e homologação real continuam exigindo avaliação.

## Build e validação

O Dockerfile verifica commit upstream e hashes dos cinco patches, copia overlay/testes, executa `go test ./...`, `go vet ./...` e compila o binário. Preserva baseline para a prova de compatibilidade. Nenhum DDL é executado na build; testes reais PostgreSQL são opt-in, com fixture descartável e guard de DSN.

Validação local: fonte final recomposta, suíte Go com race, PostgreSQL 16 isolado, módulos verificados, vet e compilação do servidor (sem executá-lo). A execução atual aprovou 463 testes/subtestes, com um skip legado PDF, mais três repetições focadas sem skips. Os registros em `.work/provisioning/` são ignorados; `.work/finalization/verified-final` foi atualizada para a fixture consumidor. Não foi construída/publicada nova imagem Docker nem homologado runtime amd64/arm64 ou conexão com contas WhatsApp. Um teste legado PDF depende de `pdftoppm`, ausente na imagem local de ferramentas.

Imagens base e pacotes Alpine não estão todos fixados por digest; a verificação de fontes/patches não significa build reproduzível bit a bit.

## Webhook existente

O patch já existente usa `WHATSAPP_WEBHOOK_SECRET` (mínimo 32 caracteres) no backend e nos emissores LembrAI. Inclui `X-LembrAI-Webhook-Secret` somente em `https://api.lembrai.jupiterti.com/api/v1/whatsapp/webhook`, rejeita redirects nesse destino e falha fechado sem segredo válido. Outros webhooks continuam sem esse header. Nunca colocar segredo na URL/repositório/log. Traefik deve preservar, não injetar o header. Esta etapa não altera a configuração existente.

## Por que o PR174 permanece fora

PR154 fecha `sqlstore.Container` no shutdown/restart controlado. PR174 propõe reutilizar o `authDB` compartilhado; combinar ambos sem adaptação pode fechar o pool de todas as instâncias. Esta candidata mantém somente PR154 para lifecycle, sem modificar sessões existentes.

## Observação

Este repositório é uma build operacional própria. Patches de upstream continuam pertencendo aos respectivos autores e devem ser revistos quando correções equivalentes forem incorporadas numa versão estável. Publicação, ativação e rollback exigem autorização expressa, preservando ledger e identidades.
