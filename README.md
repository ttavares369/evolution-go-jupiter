# evolution-go-jupiter - www.jupiterti.com

Build customizada da Evolution API GO baseada na versão oficial `0.7.2`, criada para estabilizar o ciclo de vida das instâncias e reduzir problemas de reconexão e vazamento de conexões PostgreSQL.

## Base

- Upstream: `evolution-foundation/evolution-go`
- Versão base: `0.7.2`
- Patch aplicado: PR #154 — estabilização de lifecycle, reconexão, QR, restauração de sessões e fechamento correto dos `sqlstore` containers em reinícios controlados

## Versões

`0.7.2-jupiter1` é a imagem antiga em produção; o workflow desta revisão **não a republica**. A nova versão interna é `0.7.2-jupiter2-webhook`. Somente após aprovação, o workflow publicará a tag `0.7.2-jupiter2-webhook-<commit SHA>`; registre o digest multiarch antes de configurar cada Evolution no Coolify. Nenhuma imagem é publicada por um build local.

## Por que o PR #174 não está aplicado junto

O PR #154 passa a fechar explicitamente o `sqlstore.Container` durante o shutdown/restart controlado. Já o PR #174 faz esse container reutilizar o `authDB` compartilhado. Como `Container.Close()` fecha o banco encapsulado, combinar os dois patches sem adaptação adicional poderia fechar o pool compartilhado e afetar todas as instâncias. Por isso a primeira build usa somente o #154, que já elimina o padrão de reconexões que criava pools sucessivos e garante o fechamento do pool antigo antes de um novo StartClient.

## Build

O `Dockerfile` verifica o commit da tag upstream `0.7.2` e o SHA-256 da cópia local `patches/pr154.patch` antes de aplicar `patches/lembrai-webhook.patch`. Executa os testes Go do emissor HTTP e compila a Evolution. Se a tag upstream mudar, o build falha, em vez de compilar código diferente silenciosamente. Dependências de Go, imagens base e pacotes Alpine não estão fixados por digest; portanto, a build é reproduzível quanto aos **fontes e patches**, não necessariamente bit a bit. Nenhuma alteração é feita no banco durante a build.

Para o webhook do LembrAI, configure `WHATSAPP_WEBHOOK_SECRET` (valor aleatório de pelo menos 32 caracteres) como segredo de ambiente **nas três Evolutions usadas pelo LembrAI** e no backend. O emissor inclui `X-LembrAI-Webhook-Secret` somente em `https://api.lembrai.jupiterti.com/api/v1/whatsapp/webhook`, não segue redirects nesse destino e falha sem enviar esse callback se o segredo estiver ausente ou curto. Outros webhooks continuam sem esse cabeçalho. Nunca coloque o valor na URL, no repositório ou em logs. O Traefik deve preservar, **não injetar**, o cabeçalho. Atualize os emissores um a um e valide callbacks antes de habilitar o backend que passa a exigir autenticação.

## Observação

Este repositório é uma build operacional própria. O patch continua pertencendo ao respectivo autor/upstream e deve ser removido quando uma correção equivalente for incorporada oficialmente em uma versão estável da Evolution API GO.
