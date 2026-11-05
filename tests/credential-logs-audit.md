# Etapa 6A — logs e compatibilidade legada

## Entrega final local

- `patches/credential-logs.patch`: mudanças diagnósticas em 14 fontes preexistentes, SHA-256 `c746a83caf424ebbcc35eba867919466a66fc4b5035bdfb4c4a00e600eb3e787`.
- `overlay/pkg/logger/http.go` e `http_test.go`: access log/recovery sem credenciais, preservando as respostas do Gin legado.
- `tests/credential_logs_test.go`: copiado pelo Dockerfile para `pkg/config/credential_logs_test.go`.
- `patches/queue-v1.patch`: import + uma chamada opt-in aditiva em `main`, separado do patch diagnóstico.
- Baseline imutável: upstream + PR154 + webhook, antes de logs/queue. Dockerfile preserva esse baseline para a prova AST.
- Finalização pós-6C: `patches/exclusive-instances.patch` é a exceção **funcional explícita**, separada dos diagnósticos: gate por UUID, IDOR de advanced settings e, no aditivo atual, wiring explícito do `AuthAdmin` real para adesão administrativa mínima. A prova continua revertendo o patch exato antes de comparar o legado, sem relaxar regras de autenticação. Respostas/testes do aditivo em [`../docs/queue-v1-provisioning.md`](../docs/queue-v1-provisioning.md). Ver [`../docs/queue-v1-finalization.md`](../docs/queue-v1-finalization.md). Não se alega equivalência de emissões legadas nas instâncias que aderirem à exclusividade.

Nenhum token, sessão, segredo, endpoint, resposta, payload, política de retry ou configuração de produção foi rotacionado/modificado por essas correções de logging. Não foram acessados logs de produção. Nenhum servidor Evolution foi executado.

## Riscos corrigidos

| Fonte Go | Correção diagnóstica |
| --- | --- |
| `cmd/evolution-go/main.go` | Remove token recebido em `/ws`; erros diretos de startup/banco/MinIO/RabbitMQ/migração/shutdown registram classe `%T`, não texto potencialmente sensível. `Fatal` permanece saída fatal. Instala logger/recovery seguros, sem alterar requests ou rotas. |
| `pkg/config/config.go` | Remove DSN dos logs; logger GORM específico seguro em `CreateUsersDB` não avalia SQL/parâmetros nem formata erros. Preserva nível e slow threshold. |
| `pkg/logger/logger.go` | Objetos/erros/formatters são substituídos por marcador de tipo antes de console e arquivo; não invoca `Error`, `String` ou `Format`. |
| `pkg/whatsmeow/service/whatsmeow.go` | Remove token/QR/passkey, dumps de instância/eventos, URLs assinadas e conteúdo sensível de botões/listas. Preserva UUID/evento/status/contadores. |
| `pkg/core/c0.go` | Remove prefixo/sufixo de API key, URLs de registro e texto de erro de licenciamento. |
| `pkg/events/webhook/webhook_producer.go` | Remove URL/response body e erros pré-formatados que poderiam ecoar segredos. Payloads, headers, destinos e retries não mudam. |
| `pkg/events/rabbitmq/rabbitmq_producer.go` | Remove URL de reconexão, que podia expor userinfo/path/query mesmo mascarando password; erro registra classe. |
| `pkg/events/nats/nats_producer.go` | Erros não ecoam URL/credencial. |
| `pkg/instance/repository/instance_repository.go` | Erros não ecoam SQL/parâmetros/token; queries, parâmetros e erros retornados preservados. |
| `pkg/instance/service/instance_service.go` | Remove detalhes de proxy dos logs; nenhuma mudança de proxy/credencial. |
| `pkg/utils/utils.go` | Remove valor arbitrário de cache e erros pré-formatados dos logs. |
| `pkg/storage/minio/media_storage.go` | Remove URL assinada e texto sensível de erros; URL retornada intacta. |
| `pkg/sendMessage/service/send_service.go` | Remove URL/media handle/vCard de diagnósticos; payloads e loops de retry intactos. |
| `pkg/user/service/user_service.go` | Remove URL de avatar dos logs, sem mudar retorno. |

### HTTP

`gin.Default()` registrava query completa (`/ws?token=...`), paths concretos de passkey e `c.Errors`; o recovery podia imprimir request/headers/panic/stack. Troca diagnóstica exata:

```go
r := gin.New()
r.Use(logger_wrapper.CredentialSafeHTTP(), logger_wrapper.CredentialSafeRecovery())
```

A ordem continua access log → recovery → CORS → outros middlewares. O access log inclui método de whitelist, template `c.FullPath()` (fallback estático), status e latência. Nunca URI concreta, query, IP, headers, body ou `c.Errors`. Recovery usa `gin.CustomRecoveryWithWriter(nil, ...)` e não formata o valor do panic. Preserva respostas 500/abort, respostas já iniciadas, broken pipe/reset e **o comportamento original do Gin para `http.ErrAbortHandler`**, não o substitui por nova política do net/http.

### GORM

Em `gorm v1.25.10`, `ParameterizedQueries` sozinho não protege literais SQL nem texto do erro. O logger local seguro preserva níveis/slow threshold, mas não avalia o callback SQL nem argumentos do driver. Registra evento estático/duração/classe. Não altera `gormlogger.Default`, driver, DSN, queries ou logs globais do Whatsmeow.

## Provas/testes

`TestCredentialLogsLegacyASTProof`, com `CREDENTIAL_LOGS_BASELINE=/baseline` e `EXCLUSIVE_INSTANCES_PATCH=/tmp/exclusive-instances.patch`:

- Copia os fontes Go para diretório temporário; valida e reverte **exatamente** o patch funcional com `git apply --reverse --check`/`git apply --reverse`, sem index/commits. A árvore testada e o baseline não são modificados. O hash do patch é conferido pelo Dockerfile. Sem o patch explicitamente informado, a prova falha em vez de ampliar normalizações silenciosamente.
- Sobre essa cópia sem a exceção funcional, mantém a prova original abaixo. Guards novos têm testes próprios HTTP/serviço/SQL/inventário; o mecanismo não ignora métodos inteiros ou mudanças arbitrárias de controle de fluxo.

- Compara todas as declarações preexistentes dos 14 fontes por AST.
- Normaliza apenas diagnósticos, guard de argumentos, variáveis descartadas usadas só em logs e campo diagnóstico `gorm.Config.Logger`.
- Para `main`, permite **somente pares exatos** de log antigo/novo e substituição exata Gin acima. Não ignora `setupRouter`, controle de fluxo, returns, queries, auth ou lifecycle.
- A única adição funcional aceita em main, uma vez e diretamente no corpo de `setupRouter`, é:

```go
queuev1.RegisterOptional(r, db, whatsmeowService, auth_middleware.NewMiddleware(config, instanceService).Auth, os.Getenv("EVOLUTION_QUEUE_V1") == "true")
```

- Compara os 43 outros arquivos Go preexistentes byte a byte, inclusive handlers, routes, middleware de autenticação e modelos. Imports são ignorados no AST para permitir a importação nova. Whitelists de novas declarações limitadas ao logger seguro.
- Fixtures rejeitam wiring com `AuthAdmin`, flags/args alterados, duplicação, `go`/`defer`, nesting ou alterações de respostas/assinaturas. A instalação de HTTP também tem fixtures exatas positivas/negativas.

Testes adicionais:

- Canários sintéticos em console/arquivo, INFO/WARN/ERROR/DEBUG, objetos hostis não formatados.
- Regressão que reproduz vazamento do logger GORM original com SQL/erro sintéticos e comprova proteção local.
- Driver em memória: lookup por token e updates QR/proxy recebem parâmetros intactos, sem exposição em logs.
- Gin debug/release: query, path, headers, cookies, payload, erro privado, panic, método arbitrário e rota desconhecida não expõem canários. Status/body/headers/abort comparados ao Gin legado em casos representativos.
- Webhook: autenticação no destino exato, redirects rejeitados, fail-closed, payload/fan-out preservados.
- Handlers legados texto/mídia com e sem registro queue-v1: auth/DTOs/respostas não interceptados.

Os testes compõem a suíte final do fork, executada com race e PostgreSQL descartável. Na finalização pós-6C, a prova continua aprovada; nenhum check foi removido nem relaxado por normalização genérica. Evidências/limitações de execução em [`../docs/queue-v1.md`](../docs/queue-v1.md).

## Integridade e limites

PR154 e webhook preservados:

- `fcccb9f9ad5cd40332fd96a72d41f954e7c588e5c585137d648d6996e5c628a5`
- `c163460384386317330b4420d41f8ff67a9874e01fca247624ed9f8a5b02921d`

Não é uma garantia formal de zero segredos em qualquer entrada futura. Strings escalares arbitrárias não são classificadas automaticamente; callsites conhecidos foram saneados. JIDs/números ainda podem aparecer em diagnósticos legados, sem uma política geral de anonimização. A mudança não reformula JSONs legados, que podem retornar erros de provider.

Logs internos de dependências/`WA_DEBUG`, proxies externos, histórico já gravado, backups e futuros novos callsites exigem revisão própria. Nenhuma alteração global foi aplicada ao Whatsmeow ou às credenciais existentes. Reduzir detalhes de erro/SQL nos logs é uma troca deliberada de observabilidade por segurança; preservam-se classe, evento, instância, status e duração quando aplicáveis.
