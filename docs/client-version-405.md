# Correção pontual: Client outdated (405)

## Causa e fonte

Base preservada: Evolution Go 0.7.2, commit
`9337afc47e10b86cc896a6f432240e40fee95dd1`, com Whatsmeow
`v0.0.0-20260630180629-b572e5bcb92b` e os patches existentes, inclusive PR #154.
Nenhuma dependência foi atualizada.

Comparado com [PR upstream #229](https://github.com/evolution-foundation/evolution-go/pull/229),
revisão examinada `1984ab4810967261f79706f9ac12507f95b86e11`.

O código selecionava uma versão configurada ou obtida de `web.whatsapp.com/sw.js`,
mas alterava apenas `store.DeviceProps.Version`. O handshake usa
`store.BaseClientPayload.UserAgent.AppVersion`, que permanecia na versão embutida
`2.3000.1042386815`. `store.SetWAVersion` atualiza esse campo, a versão global e o
hash de registro. Ambos os payloads nativos, de pareamento e de login, passam a
usar a versão selecionada.

## Mudança mínima

- `patches/client-version.patch` substitui somente a seleção de versão no
  `StartClient` pela inicialização compartilhada e limita a consulta web a 10 s.
- `overlay/pkg/whatsmeow/service/version.go` seleciona configuração completa e
  positiva; na ausência dela, consulta a web; se a consulta falhar, mantém a
  versão da biblioteca e registra a indisponibilidade. Não fixa outra revisão web
  que também possa envelhecer.
- `sync.Once` de processo aplica `store.SetWAVersion` e `DeviceProps.Version`
  antes de criar qualquer cliente. Depois disso os campos de versão permanecem
  imutáveis, inclusive durante reconexões. Alterar a versão ou recuperar-se de uma
  falha de consulta na inicialização requer reiniciar o processo; o fix não
  remove o risco de uma versão embutida envelhecer se a web estiver indisponível.
- O patch entra depois de PR154/webhook e antes do baseline de comparação de
  credenciais. Não há nova exceção nos testes de equivalência do código legado.

Não altera o lifecycle QR/reconexão do PR154, a escolha nativa entre registro e
login, nem instala callback de payload. Não altera sessões, credenciais,
licenciamento, queue-v1, ledger, exclusividade, validade ou retransmissões.

## Validação local

- `go test -race -json -mod=readonly -count=1 -timeout=180s ./...`, com PostgreSQL 16
  descartável sem portas expostas: **473 testes/subtestes passaram, zero falhas**.
  Um teste de thumbnail PDF foi ignorado por ausência de `pdftoppm` na imagem
  auxiliar amd64; não é um teste de versão/QR.
- `go vet -mod=readonly ./...` e build CGO amd64: passaram.
- Dockerfile ARM64 completo, executado com QEMU userspace isolado, incluindo
  `go test ./...`, `go vet ./...` e build CGO ARM64: passou. Os testes PostgreSQL
  opt-in não têm DSN nesse build; foram executados separadamente na suíte amd64.
- Novas regressões: configuração explícita, versão obtida da web e cache, falha
  de consulta, configuração parcial, AppVersion dos payloads de registro/login,
  hash de registro, 32 inicializações concorrentes, recuperação de sessão
  sintética em SQLite após reabrir o banco e canal QR com evento simulado.
- `go.mod`, `go.sum`, PR154 e todos os demais fontes Go preexistentes permanecem
  idênticos à composição anterior, exceto os trechos documentados em
  `pkg/whatsmeow/service/whatsmeow.go`.

Nenhuma conexão real ao WhatsApp foi realizada. A correção da causa demonstrada
não equivale à homologação de login/QR contra os servidores reais do WhatsApp;
essa verificação posterior, não excluir nem recriar as sessões existentes.

## Publicação

O workflow existente publica tags novas por SHA completo, para amd64 e arm64,
mais um manifesto multiarch `0.7.2-jupiter3-queue-v1-<SHA>`. Não atualizar tags
anteriores nem acionar deploy. A atualização da Oracle deve ser manual, usando o
novo digest, sem alterar a Contabo/n8n ou o SaaS.
