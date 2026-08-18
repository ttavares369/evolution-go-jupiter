# evolution-go-jupiter

Build customizada da Evolution API GO baseada na versão oficial `0.7.2`, criada para estabilizar o ciclo de vida das instâncias e reduzir problemas de reconexão e vazamento de conexões PostgreSQL.

## Base

- Upstream: `evolution-foundation/evolution-go`
- Versão base: `0.7.2`
- Patch aplicado: PR #154 — estabilização de lifecycle, reconexão, QR, restauração de sessões e fechamento correto dos `sqlstore` containers em reinícios controlados

## Tag interna

`0.7.2-jupiter1`

## Por que o PR #174 não está aplicado junto

O PR #154 passa a fechar explicitamente o `sqlstore.Container` durante o shutdown/restart controlado. Já o PR #174 faz esse container reutilizar o `authDB` compartilhado. Como `Container.Close()` fecha o banco encapsulado, combinar os dois patches sem adaptação adicional poderia fechar o pool compartilhado e afetar todas as instâncias. Por isso a primeira build usa somente o #154, que já elimina o padrão de reconexões que criava pools sucessivos e garante o fechamento do pool antigo antes de um novo StartClient.

## Build

O `Dockerfile` clona exatamente a tag `0.7.2` do upstream, aplica o PR #154 e compila a Evolution API GO. Nenhuma alteração é feita no banco durante a build.

## Observação

Este repositório é uma build operacional própria. O patch continua pertencendo ao respectivo autor/upstream e deve ser removido quando uma correção equivalente for incorporada oficialmente em uma versão estável da Evolution API GO.
