# Restauração da preparação funcional de envios no queue-v1

## Referências verificadas

- Contabo: `0.7.2-jupiter1-wa405-50af4dca9c91`, fonte
  `50af4dca9c91d95f3a98205fc2d5f89cfb4358da`.
- Oracle anterior: `0.7.2-jupiter2-webhook-wa405-1702c422018e`, fonte
  `1702c422018e0524fee2d18a31fe3cc06d674208`.
- Produtor queue-v1 anterior à correção: `0142aa9224b026266fcd5c8124989c7d922e7920`.
- Consumidor compatível, sem mudanças nesta correção:
  `9ecb1455c9348057964ffe9edf6e189fc7c7b4ad`.

As duas imagens legadas usam upstream `9337afc47e10b86cc896a6f432240e40fee95dd1`,
PR154 e Whatsmeow `v0.0.0-20260630180629-b572e5bcb92b`. A preparação legada não
foi alterada pelos hotfixes de versão/webhook. As revisões foram verificadas no
GitHub e nas attestations GHCR das imagens, sem executar instâncias reais.

## Defeito comprovado e limite do diagnóstico

O queue-v1 usava o JID solicitado sem a resolução de número/remoteJID do legado,
`Conversation` em vez de `ExtendedTextMessage` e construção própria da imagem,
sem o contexto padrão e a thumbnail do serviço existente. Essa divergência foi
eliminada. Não foi possível correlacionar os destinos e receipts dos casos reais
relatados; portanto, não se afirma que cada ausência de entrega tenha a mesma
causa nem que um teste offline com ACK comprove recebimento no aparelho.

No Whatsmeow fixado, a emissão de privacy token ocorre em goroutine depois do
envio do nó. O erro `Failed to issue privacy token ... 400` é registrado sem
fabricar nem substituir o ACK correlacionado de `SendMessage`. Não foi removida
essa proteção nem atribuída a ela uma causalidade que os dados não demonstram.

## Correção

- `pkg/sendMessage/service/message_preparation.go` contém o algoritmo extraído
  do serviço legado: consulta bruta, fallback de formatação BR/AR/MX, uso do
  remoteJID, bypass de grupo/LID e construção de texto/imagem. O serviço legado
  e queue-v1 utilizam os mesmos helpers, não duas implementações de preparação.
- O queue-v1 extrai o número de um PN JID **antes** de consultar `IsOnWhatsApp`.
  Essa dependência recebe números, não `number@s.whatsapp.net@c.us`.
- O legado mantém configuração, quoted/mentions/contexto, conexão e política de
  retries existentes. O queue-v1 não chama esses wrappers: mantém lease, prazo,
  validação, upload seguro e uma única chamada `SendMessage` com o ID do ledger.
- Texto usa `ExtendedTextMessage` e contexto padrão. Imagem usa o builder legado,
  legenda, campos de upload, thumbnail JPEG e contexto, com propriedade exclusiva
  do protobuf. O MIME validado do queue-v1 e os bytes originais são preservados.
- Presença usa a resolução compartilhada; permanece separada do envio, com os
  mesmos 8–26 segundos e refreshes. Nenhuma pausa de envio foi adicionada.
- Nenhuma alteração de schema, credencial, sessão, endpoint legado, contrato HTTP,
  hash de conteúdo, exclusividade, intervalos ou prioridades. PR154 e
  `store.SetWAVersion` permanecem inalterados.

## ACK, histórico e não duplicação

`acceptedMeans: transport_ack_not_delivery` permanece explícito. O consumidor já
exige estado `accepted`, receipt, IDs e binding válidos; não aceita apenas HTTP
200. O status `Enviado` registra aceite do transporte, não entrega/read receipt.
Não houve mudança cosmética para encobrir a regressão nem novo estado no painel.

Rejeição antes da chamada não produz receipt. Erro/timeout depois de entrar no
transporte continua incerto. Replays, reinícios e mensagens antigas aceitas ou
incertas conservam o resultado e não executam novamente a preparação/transmissão.
As retransmissões técnicas do Whatsmeow e sua identidade são preservadas.

## Validação

- Go completo, race detector e PostgreSQL 16 descartável: **548 testes/subtestes
  passaram, zero falhas**. Um teste legado de PDF foi ignorado por ausência de
  `pdftoppm` na imagem auxiliar amd64.
- `go vet` e build CGO amd64: passaram.
- Docker ARM64 completo sob QEMU userspace, com testes, vet e build CGO: passou;
  testes PG opt-in foram executados separadamente na suíte amd64.
- NestJS completo com todos os opt-ins: **90 suítes / 3215 testes aprovados, zero
  skips**, incluindo consumidor real, produtor Go/ledger/AuthAdmin/HTTP reais e
  transporte falso. TypeScript, lint e build backend também passaram.
- Novas regressões verificam destinos PN/LID, formatação, grupos, texto, PNG/JPEG/
  WebP, legenda, thumbnail e equivalência protobuf do legado. Verificam ainda
  destinatário não registrado, prazo durante lookup, cancelamento, receipt
  inválido, resultado incerto e replay/reinício sem uma segunda chamada.

Nenhuma conta/sessão/telefone real foi usado. Esses resultados não são homologação
WhatsApp real nem confirmação de entrega.

## Publicação limitada à Oracle

Publicar somente uma nova tag por SHA e digest imutável; preservar todas as tags
anteriores. Não há alteração nem publicação necessária no SaaS. Atualizar apenas
o recurso Evolution Oracle (`egal1.jupiterti.com`), mantendo suas configurações,
volumes, licença e sessões. Não atualizar Contabo/n8n nem a MASTER.

Se não houver acesso operacional ao Coolify, entregar o digest para alteração
manual **somente do campo de imagem da Oracle** e deploy desse recurso. Não
reenviar mensagens históricas, aceitas ou incertas. A confirmação final exige
recebimento observado em destinatário de teste explicitamente autorizado.
